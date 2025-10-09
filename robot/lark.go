package robot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkapplication "github.com/larksuite/oapi-sdk-go/v3/service/application/v6"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/yincongcyincong/MuseBot/conf"
	"github.com/yincongcyincong/MuseBot/db"
	"github.com/yincongcyincong/MuseBot/i18n"
	"github.com/yincongcyincong/MuseBot/logger"
	"github.com/yincongcyincong/MuseBot/param"
	"github.com/yincongcyincong/MuseBot/utils"
)

type MessageText struct {
	Text string `json:"text"`
}

var (
	cli       *larkws.Client
	BotName   string
	botClient *lark.Client
)

type LarkRobot struct {
	Message *larkim.P2MessageReceiveV1
	Robot   *RobotInfo
	Client  *lark.Client

	Command      string
	Prompt       string
	BotName      string
	ImageContent []byte
}

func StartLarkRobot(ctx context.Context) {
	eventHandler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(LarkMessageHandler)

	cli = larkws.NewClient(*conf.BaseConfInfo.LarkAPPID, *conf.BaseConfInfo.LarkAppSecret,
		larkws.WithEventHandler(eventHandler),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
		larkws.WithLogger(logger.Logger),
	)

	botClient = lark.NewClient(*conf.BaseConfInfo.LarkAPPID, *conf.BaseConfInfo.LarkAppSecret,
		lark.WithHttpClient(utils.GetRobotProxyClient()))

	// get bot name
	resp, err := botClient.Application.Application.Get(ctx, larkapplication.NewGetApplicationReqBuilder().
		AppId(*conf.BaseConfInfo.LarkAPPID).Lang("zh_cn").Build())
	if err != nil {
		logger.Error("get robot name error", "error", err)
		return
	}
	BotName = larkcore.StringValue(resp.Data.App.AppName)
	logger.Info("LarkBot Info", "username", BotName)

	err = cli.Start(ctx)
	if err != nil {
		logger.Error("start larkbot fail", "err", err)
	}
}

func NewLarkRobot(message *larkim.P2MessageReceiveV1) *LarkRobot {
	return &LarkRobot{
		Message: message,
		Client:  botClient,
		BotName: BotName,
	}
}

func LarkMessageHandler(ctx context.Context, message *larkim.P2MessageReceiveV1) error {
	l := NewLarkRobot(message)
	l.Robot = NewRobot(WithRobot(l), WithContext(ctx))
	go func() {
		defer func() {
			if err := recover(); err != nil {
				logger.Error("exec panic", "err", err, "stack", string(debug.Stack()))
			}
		}()
		l.Robot.Exec()
	}()

	return nil
}

func (l *LarkRobot) checkValid() bool {
	chatId, msgId, _ := l.Robot.GetChatIdAndMsgIdAndUserID()

	// group need to at bot
	atBot, err := l.GetMessageContent()
	if err != nil {
		logger.Error("get message content error", "err", err)
		l.Robot.SendMsg(chatId, err.Error(), msgId, "", nil)
		return false
	}
	if larkcore.StringValue(l.Message.Event.Message.ChatType) == "group" {
		if !atBot {
			logger.Warn("no at bot")
			return false
		}
	}

	logger.Info("lark exec cmd", "cmd", l.Command, "prompt", l.Prompt)

	return true
}

func (l *LarkRobot) getMsgContent() string {
	return l.Command
}

func (l *LarkRobot) requestLLMAndResp(content string) {
	if !strings.Contains(content, "/") && l.Prompt == "" {
		l.Prompt = content
	}
	l.Robot.ExecCmd(content, l.sendChatMessage)
}

func (l *LarkRobot) sendHelpConfigurationOptions() {
	data, _ := os.ReadFile("./data/117eb9b01d9f163db88.aud.mp3")
	fmt.Println(l.sendVoiceContent(data, 10))

	chatId, msgId, _ := l.Robot.GetChatIdAndMsgIdAndUserID()
	l.Robot.SendMsg(chatId, i18n.GetMessage(*conf.BaseConfInfo.Lang, "help_text", nil), msgId, tgbotapi.ModeMarkdown, nil)
}

func (l *LarkRobot) sendModeConfigurationOptions() {
	chatId, msgId, _ := l.Robot.GetChatIdAndMsgIdAndUserID()

	prompt := strings.TrimSpace(l.Prompt)
	if prompt != "" {
		if param.GeminiModels[prompt] || param.OpenAIModels[prompt] ||
			param.DeepseekModels[prompt] || param.DeepseekLocalModels[prompt] ||
			param.OpenRouterModels[prompt] || param.VolModels[prompt] {
			l.Robot.handleModeUpdate(prompt)
		}
		return
	}

	var modelList []string

	switch *conf.BaseConfInfo.Type {
	case param.DeepSeek:
		if *conf.BaseConfInfo.CustomUrl == "" || *conf.BaseConfInfo.CustomUrl == "https://api.deepseek.com/" {
			for k := range param.DeepseekModels {
				modelList = append(modelList, k)
			}
		}
	case param.Gemini:
		for k := range param.GeminiModels {
			modelList = append(modelList, k)
		}
	case param.OpenAi:
		for k := range param.OpenAIModels {
			modelList = append(modelList, k)
		}
	case param.OpenRouter, param.AI302, param.Ollama:
		if l.Prompt != "" {
			l.Robot.handleModeUpdate(l.Prompt)
			return
		}
		switch *conf.BaseConfInfo.Type {
		case param.AI302:
			l.Robot.SendMsg(chatId, i18n.GetMessage(*conf.BaseConfInfo.Lang, "mix_mode_choose", map[string]interface{}{
				"link": "https://302.ai/",
			}),
				msgId, tgbotapi.ModeMarkdown, nil)
		case param.OpenRouter:
			l.Robot.SendMsg(chatId, i18n.GetMessage(*conf.BaseConfInfo.Lang, "mix_mode_choose", map[string]interface{}{
				"link": "https://openrouter.ai/",
			}),
				msgId, tgbotapi.ModeMarkdown, nil)
		case param.Ollama:
			l.Robot.SendMsg(chatId, i18n.GetMessage(*conf.BaseConfInfo.Lang, "mix_mode_choose", map[string]interface{}{
				"link": "https://ollama.com/",
			}),
				msgId, tgbotapi.ModeMarkdown, nil)
		}

		return
	case param.Vol:
		for k := range param.VolModels {
			modelList = append(modelList, k)
		}
	}
	totalContent := ""
	for _, model := range modelList {
		totalContent += fmt.Sprintf(`%s

`, model)
	}

	l.Robot.SendMsg(chatId, totalContent, msgId, "", nil)
}

func (l *LarkRobot) sendImg() {
	l.Robot.TalkingPreCheck(func() {
		chatId, msgId, userId := l.Robot.GetChatIdAndMsgIdAndUserID()

		prompt := strings.TrimSpace(l.Prompt)
		if prompt == "" {
			logger.Warn("prompt is empty")
			l.Robot.SendMsg(chatId, i18n.GetMessage(*conf.BaseConfInfo.Lang, "photo_empty_content", nil), msgId, tgbotapi.ModeMarkdown, nil)
			return
		}

		originalMsgID := l.Robot.SendMsg(chatId, i18n.GetMessage(*conf.BaseConfInfo.Lang, "thinking", nil),
			msgId, "", nil)

		lastImageContent := l.ImageContent
		var err error
		if len(lastImageContent) == 0 && strings.Contains(l.Command, "edit_photo") {
			lastImageContent, err = l.Robot.GetLastImageContent()
			if err != nil {
				logger.Warn("get last image record fail", "err", err)
			}
		}

		imageContent, totalToken, err := l.Robot.CreatePhoto(prompt, lastImageContent)
		if err != nil {
			logger.Warn("generate image fail", "err", err)
			l.Robot.SendMsg(chatId, err.Error(), msgId, tgbotapi.ModeMarkdown, nil)
			return
		}

		base64Content := base64.StdEncoding.EncodeToString(imageContent)
		format := utils.DetectImageFormat(imageContent)
		dataURI := fmt.Sprintf("data:image/%s;base64,%s", format, base64Content)

		originImageURI := ""

		if len(l.ImageContent) > 0 {
			base64Content = base64.StdEncoding.EncodeToString(l.ImageContent)
			format = utils.DetectImageFormat(l.ImageContent)
			originImageURI = fmt.Sprintf("data:image/%s;base64,%s", format, base64Content)
		}

		resp, err := l.Client.Im.V1.Image.Create(l.Robot.Ctx, larkim.NewCreateImageReqBuilder().
			Body(larkim.NewCreateImageReqBodyBuilder().
				ImageType("message").
				Image(bytes.NewReader(imageContent)).
				Build()).
			Build())
		if err != nil || !resp.Success() {
			logger.Warn("create image fail", "err", err, "resp", resp)
			l.Robot.SendMsg(chatId, err.Error(), msgId, tgbotapi.ModeMarkdown, nil)
			return
		}

		msgContent, _ := larkim.NewMessagePost().ZhCn(larkim.NewMessagePostContent().AppendContent(
			[]larkim.MessagePostElement{
				&larkim.MessagePostImage{
					ImageKey: larkcore.StringValue(resp.Data.ImageKey),
				},
			}).Build()).Build()

		updateRes, err := l.Client.Im.Message.Update(l.Robot.Ctx, larkim.NewUpdateMessageReqBuilder().
			MessageId(originalMsgID).
			Body(larkim.NewUpdateMessageReqBodyBuilder().
				MsgType(larkim.MsgTypePost).
				Content(msgContent).
				Build()).
			Build())
		if err != nil || !updateRes.Success() {
			logger.Warn("send message fail", "err", err, "resp", resp)
			l.Robot.SendMsg(chatId, err.Error(), msgId, tgbotapi.ModeMarkdown, nil)
			return
		}

		// save data record
		db.InsertRecordInfo(&db.Record{
			UserId:     userId,
			Question:   l.Prompt,
			Answer:     dataURI,
			Content:    originImageURI,
			Token:      totalToken,
			IsDeleted:  0,
			RecordType: param.ImageRecordType,
			Mode:       *conf.BaseConfInfo.MediaType,
		})
	})
}

func (l *LarkRobot) sendVideo() {
	// 检查 prompt
	l.Robot.TalkingPreCheck(func() {
		chatId, msgId, userId := l.Robot.GetChatIdAndMsgIdAndUserID()

		prompt := strings.TrimSpace(l.Prompt)
		if prompt == "" {
			logger.Warn("prompt is empty")
			l.Robot.SendMsg(chatId, i18n.GetMessage(*conf.BaseConfInfo.Lang, "video_empty_content", nil), msgId, tgbotapi.ModeMarkdown, nil)
			return
		}

		originalMsgID := l.Robot.SendMsg(chatId, i18n.GetMessage(*conf.BaseConfInfo.Lang, "thinking", nil),
			msgId, "", nil)

		videoContent, totalToken, err := l.Robot.CreateVideo(prompt, l.ImageContent)
		if err != nil {
			logger.Warn("generate video fail", "err", err)
			l.Robot.SendMsg(chatId, err.Error(), msgId, tgbotapi.ModeMarkdown, nil)
			return
		}

		format := utils.DetectVideoMimeType(videoContent)
		resp, err := l.Client.Im.V1.File.Create(l.Robot.Ctx, larkim.NewCreateFileReqBuilder().
			Body(larkim.NewCreateFileReqBodyBuilder().
				FileType(format).
				FileName(utils.RandomFilename(format)).
				Duration(*conf.VideoConfInfo.Duration).
				File(bytes.NewReader(videoContent)).
				Build()).
			Build())
		if err != nil || !resp.Success() {
			logger.Warn("create image fail", "err", err, "resp", resp)
			l.Robot.SendMsg(chatId, err.Error(), msgId, tgbotapi.ModeMarkdown, nil)
			return
		}

		msgContent, _ := larkim.NewMessagePost().ZhCn(larkim.NewMessagePostContent().AppendContent(
			[]larkim.MessagePostElement{
				&larkim.MessagePostMedia{
					FileKey: larkcore.StringValue(resp.Data.FileKey),
				},
			}).Build()).Build()

		updateRes, err := l.Client.Im.Message.Update(l.Robot.Ctx, larkim.NewUpdateMessageReqBuilder().
			MessageId(originalMsgID).
			Body(larkim.NewUpdateMessageReqBodyBuilder().
				MsgType(larkim.MsgTypePost).
				Content(msgContent).
				Build()).
			Build())
		if err != nil || !updateRes.Success() {
			logger.Warn("send message fail", "err", err, "resp", resp)
			l.Robot.SendMsg(chatId, err.Error(), msgId, tgbotapi.ModeMarkdown, nil)
			return
		}

		base64Content := base64.StdEncoding.EncodeToString(videoContent)
		dataURI := fmt.Sprintf("data:video/%s;base64,%s", utils.DetectVideoMimeType(videoContent), base64Content)

		db.InsertRecordInfo(&db.Record{
			UserId:     userId,
			Question:   l.Prompt,
			Answer:     dataURI,
			Token:      totalToken,
			IsDeleted:  0,
			RecordType: param.VideoRecordType,
			Mode:       *conf.BaseConfInfo.MediaType,
		})
	})

}

func (l *LarkRobot) sendChatMessage() {
	l.Robot.TalkingPreCheck(func() {
		if conf.RagConfInfo.Store != nil {
			l.executeChain()
		} else {
			l.executeLLM()
		}
	})

}

func (l *LarkRobot) executeChain() {
	messageChan := &MsgChan{
		NormalMessageChan: make(chan *param.MsgInfo),
	}
	go l.Robot.ExecChain(l.Prompt, messageChan)

	go l.Robot.HandleUpdate(messageChan, "opus")
}

func (l *LarkRobot) sendText(messageChan *MsgChan) {
	var msg *param.MsgInfo
	_, messageId, _ := l.Robot.GetChatIdAndMsgIdAndUserID()
	for msg = range messageChan.NormalMessageChan {
		if len(msg.Content) == 0 {
			msg.Content = "get nothing from llm!"
		}

		if msg.MsgId == "" {
			resp, err := l.Client.Im.Message.Reply(l.Robot.Ctx, larkim.NewReplyMessageReqBuilder().
				MessageId(messageId).
				Body(larkim.NewReplyMessageReqBodyBuilder().
					MsgType(larkim.MsgTypePost).
					Content(GetMarkdownContent(msg.Content)).
					Build()).
				Build())
			if err != nil || !resp.Success() {
				logger.Warn("send message fail", "err", err, "resp", resp)
				continue
			}
			msg.MsgId = larkcore.StringValue(resp.Data.MessageId)
		} else {

			resp, err := l.Client.Im.Message.Update(l.Robot.Ctx, larkim.NewUpdateMessageReqBuilder().
				MessageId(msg.MsgId).
				Body(larkim.NewUpdateMessageReqBodyBuilder().
					MsgType(larkim.MsgTypePost).
					Content(GetMarkdownContent(msg.Content)).
					Build()).
				Build())
			if err != nil || !resp.Success() {
				logger.Warn("send message fail", "err", err, "resp", resp)
				continue
			}
		}
	}
}

func (l *LarkRobot) executeLLM() {
	messageChan := &MsgChan{
		NormalMessageChan: make(chan *param.MsgInfo),
	}
	go l.Robot.HandleUpdate(messageChan, "opus")

	go l.Robot.ExecLLM(l.Prompt, messageChan)

}

func (l *LarkRobot) getContent(content string) (string, error) {
	var err error
	msgType := larkcore.StringValue(l.Message.Event.Message.MessageType)
	_, msgId, _ := l.Robot.GetChatIdAndMsgIdAndUserID()

	switch msgType {
	case larkim.MsgTypeImage:
		msgImage := new(larkim.MessageImage)
		err = json.Unmarshal([]byte(larkcore.StringValue(l.Message.Event.Message.Content)), msgImage)
		if err != nil {
			logger.Warn("unmarshal message image failed", "err", err)
			return "", err
		}

		resp, err := l.Client.Im.V1.MessageResource.Get(l.Robot.Ctx,
			larkim.NewGetMessageResourceReqBuilder().
				MessageId(msgId).
				FileKey(msgImage.ImageKey).
				Type("image").
				Build())
		if err != nil || !resp.Success() {
			logger.Error("get image failed", "err", err, "resp", resp)
			return "", err
		}

		bs, err := io.ReadAll(resp.File)
		if err != nil {
			logger.Error("read image failed", "err", err)
			return "", err
		}

		content, err = l.Robot.GetImageContent(bs, content)
		if err != nil {
			logger.Warn("generate text from audio failed", "err", err)
			return "", err
		}
	case larkim.MsgTypeAudio:
		msgAudio := new(larkim.MessageAudio)
		err = json.Unmarshal([]byte(larkcore.StringValue(l.Message.Event.Message.Content)), msgAudio)
		if err != nil {
			logger.Warn("unmarshal message audio failed", "err", err)
			return "", err
		}
		resp, err := l.Client.Im.V1.MessageResource.Get(l.Robot.Ctx,
			larkim.NewGetMessageResourceReqBuilder().
				MessageId(msgId).
				FileKey(msgAudio.FileKey).
				Type("file").
				Build())
		if err != nil || !resp.Success() {
			logger.Error("get image failed", "err", err, "resp", resp)
			return "", err
		}

		bs, err := io.ReadAll(resp.File)
		if err != nil {
			logger.Error("read image failed", "err", err)
			return "", err
		}

		content, err = l.Robot.GetAudioContent(bs)
		if err != nil {
			logger.Warn("generate text from audio failed", "err", err)
			return "", err
		}
	case larkim.MsgTypePost:
		if len(l.ImageContent) != 0 {
			content, err = l.Robot.GetImageContent(l.ImageContent, content)
			if err != nil {
				logger.Warn("generate text from audio failed", "err", err)
				return "", err
			}
		}
	}

	if content == "" {
		logger.Warn("content extraction returned empty")
		return "", errors.New("content is empty")
	}

	return content, nil
}

func GetMarkdownContent(content string) string {
	markdownMsg, _ := larkim.NewMessagePost().ZhCn(larkim.NewMessagePostContent().AppendContent(
		[]larkim.MessagePostElement{
			&MessagePostMarkdown{
				Text: content,
			},
		}).Build()).Build()

	return markdownMsg
}

type MessagePostMarkdown struct {
	Text string `json:"text,omitempty"`
}

func (m *MessagePostMarkdown) Tag() string {
	return "md"
}

func (m *MessagePostMarkdown) IsPost() {
}

func (m *MessagePostMarkdown) MarshalJSON() ([]byte, error) {
	data := map[string]interface{}{
		"tag":  "md",
		"text": m.Text,
	}
	return json.Marshal(data)
}

type MessagePostContent struct {
	Title   string                 `json:"title"`
	Content [][]MessagePostElement `json:"content"`
}

type MessagePostElement struct {
	Tag      string `json:"tag"`
	Text     string `json:"text"`
	ImageKey string `json:"image_key"`
	UserName string `json:"user_name"`
}

func (l *LarkRobot) GetMessageContent() (bool, error) {
	_, msgId, _ := l.Robot.GetChatIdAndMsgIdAndUserID()
	msgType := larkcore.StringValue(l.Message.Event.Message.MessageType)
	botShowName := ""
	if msgType == larkim.MsgTypeText {
		textMsg := new(MessageText)
		err := json.Unmarshal([]byte(larkcore.StringValue(l.Message.Event.Message.Content)), textMsg)
		if err != nil {
			logger.Error("unmarshal text message error", "error", err)
			return false, err
		}
		l.Command, l.Prompt = ParseCommand(textMsg.Text)
		for _, at := range l.Message.Event.Message.Mentions {
			if larkcore.StringValue(at.Name) == l.BotName {
				botShowName = larkcore.StringValue(at.Key)
				break
			}
		}

		l.Prompt = strings.ReplaceAll(l.Prompt, "@"+botShowName, "")
		for _, at := range l.Message.Event.Message.Mentions {
			if larkcore.StringValue(at.Name) == l.BotName {
				botShowName = larkcore.StringValue(at.Name)
				break
			}
		}
	} else if msgType == larkim.MsgTypePost {
		postMsg := new(MessagePostContent)
		err := json.Unmarshal([]byte(larkcore.StringValue(l.Message.Event.Message.Content)), postMsg)
		if err != nil {
			logger.Error("unmarshal text message error", "error", err)
			return false, err
		}

		for _, msgPostContents := range postMsg.Content {
			for _, msgPostContent := range msgPostContents {
				switch msgPostContent.Tag {
				case "text":
					command, prompt := ParseCommand(msgPostContent.Text)
					if command != "" {
						l.Command = command
					}
					if prompt != "" {
						l.Prompt = prompt
					}
				case "img":
					resp, err := l.Client.Im.V1.MessageResource.Get(l.Robot.Ctx,
						larkim.NewGetMessageResourceReqBuilder().
							MessageId(msgId).
							FileKey(msgPostContent.ImageKey).
							Type("image").
							Build())
					if err != nil || !resp.Success() {
						logger.Error("get image failed", "err", err, "resp", resp)
						return false, err
					}

					bs, err := io.ReadAll(resp.File)
					if err != nil {
						logger.Error("read image failed", "err", err)
						return false, err
					}
					l.ImageContent = bs
				case "at":
					if l.BotName == msgPostContent.UserName {
						botShowName = msgPostContent.UserName
					}

				}
			}
		}
	}

	l.Prompt = strings.ReplaceAll(l.Prompt, "@"+l.BotName, "")
	return botShowName == l.BotName, nil
}

func (l *LarkRobot) getPrompt() string {
	return l.Prompt
}

func (l *LarkRobot) getPerMsgLen() int {
	return 4500
}

func (l *LarkRobot) sendVoiceContent(voiceContent []byte, duration int) error {
	_, messageId, _ := l.Robot.GetChatIdAndMsgIdAndUserID()

	resp, err := l.Client.Im.V1.File.Create(l.Robot.Ctx, larkim.NewCreateFileReqBuilder().
		Body(larkim.NewCreateFileReqBodyBuilder().
			FileType("opus").
			FileName(utils.RandomFilename(".ogg")).
			Duration(duration).
			File(bytes.NewReader(voiceContent)).
			Build()).
		Build())
	if err != nil || !resp.Success() {
		logger.Warn("create voice fail", "err", err, "resp", resp)
		return errors.New("request upload file fail")
	}

	audio := larkim.MessageAudio{
		FileKey: *resp.Data.FileKey,
	}
	msgContent, _ := audio.String()

	updateRes, err := l.Client.Im.Message.Reply(l.Robot.Ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(messageId).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType(larkim.MsgTypeAudio).
			Content(msgContent).
			Build()).
		Build())
	if err != nil || !updateRes.Success() {
		logger.Warn("send message fail", "err", err, "resp", updateRes)
		return errors.New("send voice fail")
	}

	return err
}
