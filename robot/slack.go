package robot

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strings"
	
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"github.com/yincongcyincong/MuseBot/conf"
	"github.com/yincongcyincong/MuseBot/db"
	"github.com/yincongcyincong/MuseBot/i18n"
	"github.com/yincongcyincong/MuseBot/logger"
	"github.com/yincongcyincong/MuseBot/metrics"
	"github.com/yincongcyincong/MuseBot/param"
	"github.com/yincongcyincong/MuseBot/utils"
)

var (
	slackClient  *slack.Client
	socketClient *socketmode.Client
	slackUserId  string
)

type SlackRobot struct {
	Event    *slackevents.MessageEvent
	CmdEvent *slack.SlashCommand
	Callback *slack.InteractionCallback
	
	Robot   *RobotInfo
	Client  *slack.Client
	Command string
	Prompt  string
	BotName string
	
	ImageContent []byte
	VoiceContent []byte
}

func StartSlackRobot(ctx context.Context) {
	if *conf.BaseConfInfo.SlackAppToken == "" || *conf.BaseConfInfo.SlackBotToken == "" {
		return
	}
	
	slackClient = slack.New(
		*conf.BaseConfInfo.SlackBotToken,
		slack.OptionDebug(false),
		slack.OptionAppLevelToken(*conf.BaseConfInfo.SlackAppToken),
		slack.OptionLog(logger.Logger),
		slack.OptionHTTPClient(utils.GetRobotProxyClient()),
	)
	socketClient = socketmode.New(slackClient)
	
	authResp, err := slackClient.AuthTest()
	if err != nil {
		logger.Error("Slack auth failed", "err", err)
		return
	}
	slackUserId = authResp.UserID
	
	go func() {
		for evt := range socketClient.Events {
			switch evt.Type {
			case socketmode.EventTypeEventsAPI:
				socketClient.Ack(*evt.Request)
				if innerEvent, ok := evt.Data.(slackevents.EventsAPIEvent); ok {
					if innerEvent.Type == slackevents.CallbackEvent {
						switch ev := innerEvent.InnerEvent.Data.(type) {
						case *slackevents.MessageEvent:
							if ev.BotID == "" && (ev.Text != "" || len(ev.Message.Files) > 0) {
								SlackMessageHandler(ev)
							}
						}
					}
				}
			case socketmode.EventTypeSlashCommand:
				cmd, ok := evt.Data.(slack.SlashCommand)
				if !ok {
					continue
				}
				socketClient.Ack(*evt.Request)
				SlackCmdHandler(&cmd)
			
			case socketmode.EventTypeInteractive:
				
				interaction, ok := evt.Data.(slack.InteractionCallback)
				if !ok {
					continue
				}
				socketClient.Ack(*evt.Request)
				
				switch interaction.Type {
				case slack.InteractionTypeBlockActions:
					SlackButtonHandler(&interaction)
				case slack.InteractionTypeViewSubmission:
					submissionHandler(&interaction)
				}
			}
		}
		
	}()
	logger.Info("SlackBot Info", "username", authResp.User)
	err = socketClient.RunContext(ctx)
	if err != nil {
		logger.Error("SlackBot Run failed", "err", err)
	}
}

func NewSlackRobot(message *slackevents.MessageEvent, command *slack.SlashCommand,
	callback *slack.InteractionCallback) *SlackRobot {
	metrics.AppRequestCount.WithLabelValues("slack").Inc()
	return &SlackRobot{
		Event:    message,
		CmdEvent: command,
		Callback: callback,
		Client:   slackClient,
	}
}

func SlackButtonHandler(callback *slack.InteractionCallback) {
	
	s := NewSlackRobot(nil, nil, callback)
	s.Robot = NewRobot(WithRobot(s))
	
	for _, action := range callback.ActionCallback.BlockActions {
		s.Command = action.ActionID
		switch action.ActionID {
		case "chat", "photo", "video", "mcp", "task":
			s.openModal(callback.TriggerID, action.ActionID)
		default:
			s.Robot.ExecCmd(s.Command, nil, nil, nil)
			
		}
	}
}

func SlackCmdHandler(command *slack.SlashCommand) {
	s := NewSlackRobot(nil, command, nil)
	s.Robot = NewRobot(WithRobot(s))
	go func() {
		defer func() {
			if err := recover(); err != nil {
				logger.Error("Slack exec panic", "err", err, "stack", string(debug.Stack()))
			}
		}()
		
		s.Command = command.Command
		s.Prompt = command.Text
		s.Robot.AddUserInfo()
		s.Robot.ExecCmd(s.Command, s.sendChatMessage, nil, nil)
		
	}()
}

func SlackMessageHandler(message *slackevents.MessageEvent) {
	r := NewSlackRobot(message, nil, nil)
	r.Robot = NewRobot(WithRobot(r))
	go func() {
		defer func() {
			if err := recover(); err != nil {
				logger.Error("Slack exec panic", "err", err, "stack", string(debug.Stack()))
			}
		}()
		r.Robot.Exec()
	}()
}

func (s *SlackRobot) checkValid() bool {
	// group need at bot
	atRobot := fmt.Sprintf("<@%s>", slackUserId)
	if (strings.HasPrefix(s.Event.Channel, "C") || strings.HasPrefix(s.Event.Channel, "G")) &&
		strings.Contains(s.Event.Text, atRobot) {
		return false
	}
	
	s.Command, s.Prompt = ParseCommand(strings.ReplaceAll(s.Event.Text, atRobot, ""))
	s.getMessageContent()
	return true
}

func (s *SlackRobot) getMessageContent() {
	if s.Event != nil && s.Event.Message.Files != nil && len(s.Event.Message.Files) > 0 {
		file := s.Event.Message.Files[0]
		var err error
		
		switch file.Mimetype {
		case "image/jpeg", "image/png", "image/gif", "image/webp":
			s.ImageContent, err = s.downloadSlackFile(file.URLPrivateDownload)
			if err != nil {
				logger.Error("download image failed", "err", err)
				return
			}
		
		case "audio/mpeg", "audio/wav", "audio/ogg", "audio/mp4":
			// 下载音频
			s.VoiceContent, err = s.downloadSlackFile(file.URLPrivateDownload)
			if err != nil {
				logger.Error("download audio failed", "err", err)
				return
			}
		}
	}
}

func (s *SlackRobot) getMsgContent() string {
	return s.Command
}

func (s *SlackRobot) requestLLM(content string) {
	if !strings.Contains(content, "/") && !strings.Contains(content, "$") && s.Prompt == "" {
		s.Prompt = content
	}
	s.Robot.ExecCmd(content, s.sendChatMessage, nil, nil)
}

func (s *SlackRobot) sendChatMessage() {
	s.Robot.TalkingPreCheck(func() {
		if conf.RagConfInfo.Store != nil {
			s.executeChain(s.Prompt)
		} else {
			s.executeLLM(s.Prompt)
		}
	})
	
}

func (s *SlackRobot) executeChain(content string) {
	messageChan := &MsgChan{
		NormalMessageChan: make(chan *param.MsgInfo),
	}
	go s.Robot.ExecChain(content, messageChan)
	
	go s.Robot.HandleUpdate(messageChan, "mp3")
}

func (s *SlackRobot) executeLLM(content string) {
	messageChan := &MsgChan{
		NormalMessageChan: make(chan *param.MsgInfo),
	}
	go s.Robot.ExecLLM(content, messageChan)
	
	go s.Robot.HandleUpdate(messageChan, "mp3")
}

func (s *SlackRobot) getContent(content string) (string, error) {
	if len(s.Event.Message.Files) == 0 {
		return content, nil
	}
	
	file := s.Event.Message.Files[0]
	var err error
	
	switch file.Mimetype {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		content, err = s.Robot.GetImageContent(s.ImageContent, content)
		if err != nil {
			logger.Warn("generate text from image failed", "err", err)
			return "", err
		}
	
	case "audio/mpeg", "audio/wav", "audio/ogg", "audio/mp4":
		content, err = s.Robot.GetAudioContent(s.VoiceContent)
		if err != nil {
			logger.Warn("generate text from audio failed", "err", err)
			return "", err
		}
	default:
		return "", fmt.Errorf("unsupported file type: %s", file.Mimetype)
	}
	
	if content == "" {
		logger.Warn("content extraction returned empty")
		return "", errors.New("content is empty")
	}
	
	return content, nil
}

func (s *SlackRobot) downloadSlackFile(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+*conf.BaseConfInfo.SlackBotToken)
	
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download file: %s", resp.Status)
	}
	
	return io.ReadAll(resp.Body)
}

func (s *SlackRobot) sendImg() {
	s.Robot.TalkingPreCheck(func() {
		chatId, msgId, userId := s.Robot.GetChatIdAndMsgIdAndUserID()
		
		prompt := s.Prompt
		prompt = utils.ReplaceCommand(prompt, "/photo", s.BotName)
		if prompt == "" {
			logger.Warn("prompt is empty")
			s.Robot.SendMsg(chatId, i18n.GetMessage(*conf.BaseConfInfo.Lang, "photo_empty_content", nil), msgId, tgbotapi.ModeMarkdown, nil)
			return
		}
		
		thinkingMsg := s.Robot.SendMsg(chatId, i18n.GetMessage(*conf.BaseConfInfo.Lang, "thinking", nil), msgId, "", nil)
		
		var err error
		lastImageContent := s.ImageContent
		if len(lastImageContent) == 0 && strings.Contains(s.Command, "edit_photo") {
			lastImageContent, err = s.Robot.GetLastImageContent()
			if err != nil {
				logger.Warn("get last image record fail", "err", err)
			}
		}
		
		imageContent, totalToken, err := s.Robot.CreatePhoto(prompt, lastImageContent)
		if err != nil {
			logger.Warn("generate image fail", "err", err)
			s.Robot.SendMsg(chatId, err.Error(), msgId, tgbotapi.ModeMarkdown, nil)
			return
		}
		
		uploadParams := slack.UploadFileV2Parameters{
			Filename: "image." + utils.DetectImageFormat(imageContent),
			Reader:   bytes.NewReader(imageContent),
			Title:    "image",
			FileSize: len(imageContent),
			Channel:  chatId,
		}
		
		_, err = s.Client.UploadFileV2(uploadParams)
		if err != nil {
			logger.Warn("upload image to slack fail", "err", err)
			s.Robot.SendMsg(chatId, err.Error(), msgId, tgbotapi.ModeMarkdown, nil)
			return
		}
		
		if thinkingMsg != "" {
			_, _, err := s.Client.DeleteMessage(chatId, thinkingMsg)
			if err != nil {
				logger.Warn("delete thinking message fail", "err", err)
			}
		}
		
		originImageURI := ""
		if len(lastImageContent) > 0 {
			base64Content := base64.StdEncoding.EncodeToString(lastImageContent)
			format := utils.DetectImageFormat(lastImageContent)
			originImageURI = fmt.Sprintf("data:image/%s;base64,%s", format, base64Content)
		}
		
		dataURI := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(imageContent)
		db.InsertRecordInfo(&db.Record{
			UserId:     userId,
			Question:   prompt,
			Answer:     dataURI,
			Content:    originImageURI,
			Token:      totalToken,
			IsDeleted:  0,
			RecordType: param.ImageRecordType,
			Mode:       utils.GetImgType(db.GetCtxUserInfo(s.Robot.Ctx).LLMConfigRaw),
		})
	})
}

func (s *SlackRobot) sendVideo() {
	s.Robot.TalkingPreCheck(func() {
		chatId, replyToMessageID, userID := s.Robot.GetChatIdAndMsgIdAndUserID()
		
		prompt := s.Prompt
		prompt = utils.ReplaceCommand(prompt, "/video", s.BotName)
		if prompt == "" {
			logger.Warn("prompt is empty")
			s.Robot.SendMsg(chatId, i18n.GetMessage(*conf.BaseConfInfo.Lang, "video_empty_content", nil),
				replyToMessageID, tgbotapi.ModeMarkdown, nil)
			return
		}
		
		thinkingMsg := s.Robot.SendMsg(chatId, i18n.GetMessage(*conf.BaseConfInfo.Lang, "thinking", nil), replyToMessageID, "", nil)
		
		var err error
		lastImageContent := s.ImageContent
		videoContent, totalToken, err := s.Robot.CreateVideo(prompt, lastImageContent)
		if err != nil {
			logger.Warn("generate video failed", "err", err)
			s.Robot.SendMsg(chatId, err.Error(), replyToMessageID, tgbotapi.ModeMarkdown, nil)
			return
		}
		
		uploadParams := slack.UploadFileV2Parameters{
			Filename: "video." + utils.DetectVideoMimeType(videoContent),
			Reader:   bytes.NewReader(videoContent),
			Title:    "video",
			FileSize: len(videoContent),
			Channel:  chatId,
		}
		
		_, err = s.Client.UploadFileV2(uploadParams)
		if err != nil {
			logger.Warn("upload image to slack fail", "err", err)
			s.Robot.SendMsg(chatId, err.Error(), replyToMessageID, tgbotapi.ModeMarkdown, nil)
			return
		}
		
		if thinkingMsg != "" {
			_, _, err := s.Client.DeleteMessage(chatId, thinkingMsg)
			if err != nil {
				logger.Warn("delete thinking message fail", "err", err)
			}
		}
		
		// 记录到数据库
		base64Content := base64.StdEncoding.EncodeToString(videoContent)
		dataURI := fmt.Sprintf("data:video/%s;base64,%s", utils.DetectVideoMimeType(videoContent), base64Content)
		
		originImageURI := ""
		if len(lastImageContent) > 0 {
			base64Content = base64.StdEncoding.EncodeToString(lastImageContent)
			format := utils.DetectImageFormat(lastImageContent)
			originImageURI = fmt.Sprintf("data:image/%s;base64,%s", format, base64Content)
		}
		
		db.InsertRecordInfo(&db.Record{
			UserId:     userID,
			Question:   prompt,
			Answer:     dataURI,
			Content:    originImageURI,
			Token:      totalToken,
			IsDeleted:  0,
			RecordType: param.VideoRecordType,
			Mode:       utils.GetVideoType(db.GetCtxUserInfo(s.Robot.Ctx).LLMConfigRaw),
		})
	})
}

func (s *SlackRobot) sendHelpConfigurationOptions() {
	chatId, _, _ := s.Robot.GetChatIdAndMsgIdAndUserID()
	
	blocks := []slack.Block{
		slack.NewActionBlock("action_block",
			slack.NewButtonBlockElement("mode", "mode", slack.NewTextBlockObject("plain_text", "mode", false, false)),
			slack.NewButtonBlockElement("clear", "clear", slack.NewTextBlockObject("plain_text", "clear", false, false)),
		),
		slack.NewActionBlock("action_block2",
			slack.NewButtonBlockElement("balance", "balance", slack.NewTextBlockObject("plain_text", "balance", false, false)),
			slack.NewButtonBlockElement("state", "state", slack.NewTextBlockObject("plain_text", "state", false, false)),
		),
		slack.NewActionBlock("action_block3",
			slack.NewButtonBlockElement("retry", "retry", slack.NewTextBlockObject("plain_text", "retry", false, false)),
			slack.NewButtonBlockElement("chat", "chat", slack.NewTextBlockObject("plain_text", "chat", false, false)),
		),
		slack.NewActionBlock("action_block4",
			slack.NewButtonBlockElement("photo", "photo", slack.NewTextBlockObject("plain_text", "photo", false, false)),
			slack.NewButtonBlockElement("video", "video", slack.NewTextBlockObject("plain_text", "video", false, false)),
		),
	}
	
	_, _, err := s.Client.PostMessage(chatId, slack.MsgOptionBlocks(blocks...))
	if err != nil {
		logger.Error("post message failed", "err", err)
	}
}

func (s *SlackRobot) openModal(triggerID, actionID string) {
	chatId, _, _ := s.Robot.GetChatIdAndMsgIdAndUserID()
	modalRequest := slack.ModalViewRequest{
		Type:            slack.VTModal,
		Title:           slack.NewTextBlockObject("plain_text", "prompt", false, false),
		Close:           slack.NewTextBlockObject("plain_text", "cancel", false, false),
		Submit:          slack.NewTextBlockObject("plain_text", "submit", false, false),
		CallbackID:      chatId,
		PrivateMetadata: actionID,
		Blocks: slack.Blocks{
			BlockSet: []slack.Block{
				slack.NewInputBlock(
					"input_block",
					slack.NewTextBlockObject("plain_text", "input prompt", false, false),
					slack.NewTextBlockObject("plain_text", "input prompt", false, false),
					slack.NewPlainTextInputBlockElement(slack.NewTextBlockObject("plain_text", "prompt...", false, false), "input_action"),
				),
			},
		},
	}
	
	_, err := s.Client.OpenView(triggerID, modalRequest)
	if err != nil {
		logger.Error("open modal failed", "err", err)
	}
}

func submissionHandler(callback *slack.InteractionCallback) {
	s := NewSlackRobot(nil, nil, callback)
	s.Robot = NewRobot(WithRobot(s))
	
	s.Command = callback.View.PrivateMetadata
	values := callback.View.State.Values
	inputBlock := values["input_block"]
	for _, v := range inputBlock {
		s.Prompt += v.Value
	}
	s.Callback.Channel.ID = callback.View.CallbackID
	
	s.Robot.AddUserInfo()
	s.getMessageContent()
	s.Robot.ExecCmd(s.Command, nil, nil, nil)
	
}

func (s *SlackRobot) getPrompt() string {
	return s.Prompt
}

func (s *SlackRobot) getPerMsgLen() int {
	return 1800
}

func (s *SlackRobot) sendText(messageChan *MsgChan) {
	chatId, messageId, _ := s.Robot.GetChatIdAndMsgIdAndUserID()
	
	for msg := range messageChan.NormalMessageChan {
		if msg.Content == "" {
			msg.Content = "get nothing from llm!"
		}
		
		if msg.MsgId == "" {
			_, newMsgTimestamp, err := s.Client.PostMessage(
				chatId,
				slack.MsgOptionText(msg.Content, false),
				slack.MsgOptionTS(messageId),
			)
			if err != nil {
				logger.Error("send new message failed", "err", err)
				continue
			}
			msg.MsgId = newMsgTimestamp
		} else {
			_, _, _, err := s.Client.UpdateMessage(
				chatId,
				msg.MsgId,
				slack.MsgOptionText(msg.Content, false),
				slack.MsgOptionTS(messageId),
			)
			if err != nil {
				logger.Error("update message failed", "err", err)
				continue
			}
		}
	}
}

func (s *SlackRobot) sendVoiceContent(voiceContent []byte, duration int) error {
	chatId, _, _ := s.Robot.GetChatIdAndMsgIdAndUserID()
	format := utils.DetectAudioFormat(voiceContent)
	uploadParams := slack.UploadFileV2Parameters{
		Filename: "voice." + format,
		Reader:   bytes.NewReader(voiceContent),
		Title:    "voice",
		FileSize: len(voiceContent),
		Channel:  chatId,
	}
	
	_, err := s.Client.UploadFileV2(uploadParams)
	if err != nil {
		logger.Warn("upload voice to slack fail", "err", err)
		return err
	}
	
	return nil
}

func (s *SlackRobot) setCommand(command string) {
	s.Command = command
}

func (s *SlackRobot) getCommand() string {
	return s.Command
}
