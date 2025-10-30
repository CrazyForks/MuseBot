package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	
	"github.com/cohesion-org/deepseek-go/constants"
	openrouter "github.com/revrost/go-openrouter"
	"github.com/yincongcyincong/MuseBot/conf"
	"github.com/yincongcyincong/MuseBot/db"
	"github.com/yincongcyincong/MuseBot/i18n"
	"github.com/yincongcyincong/MuseBot/logger"
	"github.com/yincongcyincong/MuseBot/metrics"
	"github.com/yincongcyincong/MuseBot/param"
	"github.com/yincongcyincong/MuseBot/utils"
)

var (
	pngFetch = regexp.MustCompile(`https?://[^\s)]+\.png[^\s)]*`)
)

// AI302FetchResp fetch response
type AI302FetchResp struct {
	TaskID         string           `json:"task_id"`
	UpstreamTaskID string           `json:"upstream_task_id"`
	Status         string           `json:"status"`
	VideoURL       string           `json:"video_url"`
	RawResponse    AI302RawResponse `json:"raw_response"`
	Model          string           `json:"model"`
	ExecutionTime  int              `json:"execution_time"`
	CreatedAt      *string          `json:"created_at"`
	CompletedAt    string           `json:"completed_at"`
}

type AI302RawResponse struct {
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
	Prompt    string `json:"prompt"`
	State     string `json:"state"`
	Video     string `json:"video"`
}

// Create video response
type CreateResp struct {
	TaskID string `json:"task_id"`
}

func GenerateMixImg(ctx context.Context, prompt string, imageContent []byte) (string, int, error) {
	start := time.Now()
	llmConfig := db.GetCtxUserInfo(ctx).LLMConfigRaw
	mediaType := utils.GetImgType(llmConfig)
	model := utils.GetUsingImgModel(mediaType, llmConfig.ImgModel)
	metrics.APIRequestCount.WithLabelValues(model).Inc()
	
	messages := openrouter.ChatCompletionMessage{
		Role: constants.ChatMessageRoleUser,
		Content: openrouter.Content{
			Multi: []openrouter.ChatMessagePart{
				{
					Type: openrouter.ChatMessagePartTypeText,
					Text: prompt,
				},
			},
		},
	}
	
	if len(imageContent) != 0 {
		messages.Content.Multi = append(messages.Content.Multi, openrouter.ChatMessagePart{
			Type: openrouter.ChatMessagePartTypeImageURL,
			ImageURL: &openrouter.ChatMessageImageURL{
				URL: "data:image/" + utils.DetectImageFormat(imageContent) + ";base64," + base64.StdEncoding.EncodeToString(imageContent),
			},
		})
	}
	
	client := GetMixClient(ctx, "img")
	request := openrouter.ChatCompletionRequest{
		Model:    model,
		Messages: []openrouter.ChatCompletionMessage{messages},
	}
	
	// assign task
	response, err := client.CreateChatCompletion(ctx, request)
	
	metrics.APIRequestDuration.WithLabelValues(model).Observe(time.Since(start).Seconds())
	
	if err != nil {
		logger.ErrorCtx(ctx, "create chat completion fail", "err", err)
		return "", 0, err
	}
	
	if len(response.Choices) != 0 {
		if mediaType == param.AI302 {
			pngs := pngFetch.FindAllString(response.Choices[0].Message.Content.Text, -1)
			return pngs[len(pngs)-1], response.Usage.TotalTokens, nil
		} else if mediaType == param.OpenRouter {
			if len(response.Choices[0].Message.Content.Multi) != 0 {
				return response.Choices[0].Message.Content.Multi[0].ImageURL.URL, response.Usage.TotalTokens, nil
			}
		}
	}
	
	return "", 0, errors.New("image is empty")
}

func GetMixClient(ctx context.Context, clientType string) *openrouter.Client {
	t := param.OpenRouter
	switch clientType {
	case "txt":
		t = utils.GetTxtType(db.GetCtxUserInfo(ctx).LLMConfigRaw)
	case "img":
		t = utils.GetImgType(db.GetCtxUserInfo(ctx).LLMConfigRaw)
	case "video":
		t = utils.GetVideoType(db.GetCtxUserInfo(ctx).LLMConfigRaw)
	case "rec":
		t = utils.GetRecType(db.GetCtxUserInfo(ctx).LLMConfigRaw)
	}
	
	token := ""
	specialLLMUrl := ""
	switch t {
	case param.OpenRouter:
		token = *conf.BaseConfInfo.OpenRouterToken
	case param.AI302:
		token = *conf.BaseConfInfo.AI302Token
		specialLLMUrl = "https://api.302.ai/"
	}
	
	config := openrouter.DefaultConfig(token)
	config.HTTPClient = utils.GetLLMProxyClient()
	if specialLLMUrl != "" {
		config.BaseURL = specialLLMUrl
	}
	if *conf.BaseConfInfo.CustomUrl != "" {
		config.BaseURL = *conf.BaseConfInfo.CustomUrl
	}
	return openrouter.NewClientWithConfig(*config)
}

func Generate302AIVideo(ctx context.Context, prompt string, image []byte) (string, int, error) {
	httpClient := utils.GetLLMProxyClient()
	
	start := time.Now()
	llmConfig := db.GetCtxUserInfo(ctx).LLMConfigRaw
	mediaType := utils.GetVideoType(llmConfig)
	model := utils.GetUsingVideoModel(mediaType, llmConfig.VideoModel)
	metrics.APIRequestCount.WithLabelValues(model).Inc()
	
	// Step 1: prepare payload using map -> json
	payloadMap := map[string]interface{}{
		"model":      model,
		"prompt":     prompt,
		"duration":   *conf.VideoConfInfo.Duration,
		"resolution": *conf.VideoConfInfo.Resolution,
		"fps":        *conf.VideoConfInfo.FPS,
	}
	
	payloadBytes, err := json.Marshal(payloadMap)
	if err != nil {
		return "", 0, fmt.Errorf("failed to marshal payload: %w", err)
	}
	payload := strings.NewReader(string(payloadBytes))
	
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.302.ai/302/v2/video/create", payload)
	
	metrics.APIRequestDuration.WithLabelValues(model).Observe(time.Since(start).Seconds())
	
	if err != nil {
		return "", 0, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Add("Authorization", "Bearer "+*conf.BaseConfInfo.AI302Token)
	req.Header.Add("Content-Type", "application/json")
	
	res, err := httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("failed to call create API: %w", err)
	}
	defer res.Body.Close()
	
	body, _ := io.ReadAll(res.Body)
	
	var createResp CreateResp
	if err := json.Unmarshal(body, &createResp); err != nil {
		return "", 0, fmt.Errorf("failed to parse create response: %w, body=%s", err, string(body))
	}
	if createResp.TaskID == "" {
		return "", 0, fmt.Errorf("no task_id returned from create API, body=%s", string(body))
	}
	
	// Step 2: Poll fetch API (保持原逻辑)
	fetchURL := "https://api.302.ai/302/v2/video/fetch/" + createResp.TaskID
	for i := 0; i < 100; i++ {
		select {
		case <-ctx.Done():
			return "", 0, fmt.Errorf("context canceled or timeout: %w", ctx.Err())
		default:
		}
		
		req, _ := http.NewRequestWithContext(ctx, "GET", fetchURL, nil)
		req.Header.Add("Authorization", "Bearer "+*conf.BaseConfInfo.AI302Token)
		
		res, err := httpClient.Do(req)
		if err != nil {
			logger.ErrorCtx(ctx, "failed to fetch result:", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		
		var fetchResp AI302FetchResp
		if err := json.Unmarshal(body, &fetchResp); err != nil {
			logger.ErrorCtx(ctx, "failed to parse fetch response:", "err", err, "body", string(body))
			time.Sleep(5 * time.Second)
			continue
		}
		
		if fetchResp.Status == "completed" {
			if fetchResp.VideoURL != "" {
				return fetchResp.VideoURL, 0, nil
			}
			return "", 0, fmt.Errorf("task completed but no video url found, body=%s", string(body))
		} else if fetchResp.Status == "failed" {
			return "", 0, fmt.Errorf("video generation failed: body=%s", string(body))
		} else {
			logger.InfoCtx(ctx, "task is still running, polling again...")
		}
		
		time.Sleep(5 * time.Second)
	}
	
	return "", 0, fmt.Errorf("video generation timeout")
}

func GetMixImageContent(ctx context.Context, imageContent []byte, content string) (string, int, error) {
	contentPrompt := content
	if content == "" {
		contentPrompt = i18n.GetMessage(*conf.BaseConfInfo.Lang, "photo_handle_prompt", nil)
	}
	
	start := time.Now()
	llmConfig := db.GetCtxUserInfo(ctx).LLMConfigRaw
	mediaType := utils.GetRecType(llmConfig)
	model := utils.GetUsingRecModel(mediaType, llmConfig.RecModel)
	metrics.APIRequestCount.WithLabelValues(model).Inc()
	
	messages := openrouter.ChatCompletionMessage{
		Role: constants.ChatMessageRoleUser,
		Content: openrouter.Content{
			Multi: []openrouter.ChatMessagePart{
				{
					Type: openrouter.ChatMessagePartTypeText,
					Text: contentPrompt,
				},
				{
					Type: openrouter.ChatMessagePartTypeImageURL,
					ImageURL: &openrouter.ChatMessageImageURL{
						URL: "data:image/" + utils.DetectImageFormat(imageContent) + ";base64," + base64.StdEncoding.EncodeToString(imageContent),
					},
				},
			},
		},
	}
	
	client := GetMixClient(ctx, "rec")
	
	request := openrouter.ChatCompletionRequest{
		Model:    model,
		Messages: []openrouter.ChatCompletionMessage{messages},
	}
	
	// assign task
	response, err := client.CreateChatCompletion(ctx, request)
	
	metrics.APIRequestDuration.WithLabelValues(model).Observe(time.Since(start).Seconds())
	
	if err != nil {
		logger.ErrorCtx(ctx, "create chat completion fail", "err", err)
		return "", 0, err
	}
	
	if len(response.Choices) == 0 {
		logger.ErrorCtx(ctx, "response is emtpy", "response", response)
		return "", 0, errors.New("response is empty")
	}
	
	return response.Choices[0].Message.Content.Text, response.Usage.TotalTokens, nil
}
