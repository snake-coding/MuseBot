package robot

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkapplication "github.com/larksuite/oapi-sdk-go/v3/service/application/v6"
	larkcardkit "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"
	larkcontact "github.com/larksuite/oapi-sdk-go/v3/service/contact/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
	"github.com/yincongcyincong/MuseBot/conf"
	"github.com/yincongcyincong/MuseBot/i18n"
	"github.com/yincongcyincong/MuseBot/logger"
	"github.com/yincongcyincong/MuseBot/param"
	"github.com/yincongcyincong/MuseBot/utils"
)

type MessageText struct {
	Text string `json:"text"`
}

// LarkBotInstance 封装单个飞书机器人的运行状态
type LarkBotInstance struct {
	AppID     string
	AppSecret string
	Client    *lark.Client
	BotName   string
}

// 全局默认实例（用于兼容旧配置启动）
var defaultLarkInstance *LarkBotInstance

// 兼容旧代码的全局变量
var (
	LarkBotClient *lark.Client
	BotName       string
)

type LarkRobot struct {
	Message *larkim.P2MessageReceiveV1
	Robot   *RobotInfo
	Client  *lark.Client

	Command      string
	Prompt       string
	BotName      string
	ImageContent []byte
	AudioContent []byte
	UserName     string
}

func StartLarkRobot(ctx context.Context) {
	if conf.BaseConfInfo.LarkAPPID == "" || conf.BaseConfInfo.LarkAppSecret == "" {
		return
	}
	RunLarkRobot(ctx, conf.BaseConfInfo.LarkAPPID, conf.BaseConfInfo.LarkAppSecret)
}

func RunLarkRobot(ctx context.Context, appID, appSecret string) {
	instance := &LarkBotInstance{
		AppID:     appID,
		AppSecret: appSecret,
		Client: lark.NewClient(appID, appSecret,
			lark.WithHttpClient(utils.GetRobotProxyClient())),
	}

	nameResp, err := instance.Client.Application.Application.Get(ctx, larkapplication.NewGetApplicationReqBuilder().
		AppId(appID).Lang("zh_cn").Build())
	if err == nil && nameResp.Success() {
		instance.BotName = larkcore.StringValue(nameResp.Data.App.AppName)
	} else {
		instance.BotName = "LarkBot"
		logger.ErrorCtx(ctx, "get robot name error", "error", err, "resp", nameResp)
	}

	if appID == conf.BaseConfInfo.LarkAPPID {
		defaultLarkInstance = instance
		LarkBotClient = instance.Client
		BotName = instance.BotName
	}

	eventHandler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(ctx context.Context, message *larkim.P2MessageReceiveV1) error {
			if message.Event == nil || message.Event.Message == nil {
				return nil
			}

			l := &LarkRobot{
				Message: message,
				Client:  instance.Client,
				BotName: instance.BotName,
			}
			l.Robot = NewRobot(WithRobot(l), WithContext(ctx))

			go func() {
				defer func() {
					if err := recover(); err != nil {
						logger.ErrorCtx(ctx, "exec panic", "err", err, "stack", string(debug.Stack()))
					}
				}()

				if message.Event.Sender != nil && message.Event.Sender.SenderId != nil && message.Event.Sender.SenderId.UserId != nil {
					userInfo, err := l.Client.Contact.V3.User.Get(l.Robot.Ctx, larkcontact.NewGetUserReqBuilder().
						UserId(*message.Event.Sender.SenderId.UserId).UserIdType("user_id").Build())
					if err == nil && userInfo != nil && userInfo.Code == 0 && userInfo.Data != nil && userInfo.Data.User != nil {
						l.UserName = larkcore.StringValue(userInfo.Data.User.Name)
					}
				}

				l.Robot.Exec()
			}()
			return nil
		})

	wsClient := larkws.NewClient(appID, appSecret,
		larkws.WithEventHandler(eventHandler),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
		larkws.WithLogger(logger.Logger),
	)

	logger.Info("LarkBot Started", "appID", appID, "username", instance.BotName)

	err = wsClient.Start(ctx)
	if err != nil {
		logger.ErrorCtx(ctx, "start larkbot fail", "err", err)
	}
}

func (l *LarkRobot) checkValid() bool {
	if l.Message == nil || l.Message.Event == nil || l.Message.Event.Message == nil {
		return false
	}
	chatId, msgId, _ := l.Robot.GetChatIdAndMsgIdAndUserID()
	atBot, err := l.GetMessageContent()
	if err != nil {
		logger.ErrorCtx(l.Robot.Ctx, "get message content error", "err", err)
		l.Robot.SendMsg(chatId, err.Error(), msgId, "", nil)
		return false
	}
	if larkcore.StringValue(l.Message.Event.Message.ChatType) == "group" {
		if !atBot {
			return false
		}
	}
	return true
}

func (l *LarkRobot) getMsgContent() string {
	return l.Command + " " + l.Prompt
}

func (l *LarkRobot) requestLLM(content string) {
	l.requestCustomSSE(content)
}

type CustomSSEResponse struct {
	Status      string `json:"status"`
	MessageType string `json:"message_type"`
	Content     struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (l *LarkRobot) requestCustomSSE(prompt string) {
	message_id := uuid.New().String()
	l.Robot.TalkingPreCheck(func() {
		// 1. 创建初始卡片
		cardID, err := l.createInitialStreamingCard()
		if err != nil {
			logger.ErrorCtx(l.Robot.Ctx, "create card fail", "err", err)
			return
		}

		// 2. 发送回复
		if err := l.replyWithCardID(cardID); err != nil {
			return
		}

		// 3. SSE 请求
		apiURL := "https://10.106.144.215/api/com-qt-app-aiagent/ai-agent-data/console/v1/sessions/758782a2-aa56-4c4e-aa5f-888b7a864f82/chat/stream"
		reqBody := map[string]interface{}{
			"user_input": prompt,
			"message_id": message_id,
		}
		jsonBody, _ := json.Marshal(reqBody)

		req, err := http.NewRequestWithContext(l.Robot.Ctx, "POST", apiURL, bytes.NewBuffer(jsonBody))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cookie", "QTSID=OFADIRSFZAqE54bLA868hPHmjCw9gSSgskYNixouWRg")

		client := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
			Timeout: 5 * time.Minute,
		}
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()

		reader := bufio.NewReader(resp.Body)
		var thinkingContent strings.Builder
		var answerContent strings.Builder
		sequence := 1
		isThinkingDone := false

		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				break
			}

			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}

			jsonStr := strings.TrimPrefix(line, "data:")
			var sseResp CustomSSEResponse
			if err := json.Unmarshal([]byte(jsonStr), &sseResp); err != nil {
				continue
			}

			if sseResp.MessageType == "thinking" {
				thinkingContent.WriteString(sseResp.Content.Text)
				if sseResp.Status == "finish" {
					// 思考结束：更新卡片结构，添加折叠面板
					l.transitionToAnswerPhase(cardID, thinkingContent.String(), sequence)
					isThinkingDone = true
				} else {
					// 思考中：流式更新第一个文本框
					l.updateStreamingElement(cardID, "streaming_txt", thinkingContent.String(), sequence)
				}
			} else if sseResp.MessageType == "answer" {
				if !isThinkingDone {
					// 兜底：如果没收到 thinking finish 就收到了 answer
					l.transitionToAnswerPhase(cardID, thinkingContent.String(), sequence)
					isThinkingDone = true
				}
				answerContent.WriteString(sseResp.Content.Text)
				if sseResp.Status == "finish" {
					l.finishResponse(cardID, answerContent.String(), sequence)
				}
				// else {
				// 	// l.updateStreamingElement(cardID, "answer_txt", answerContent.String(), sequence)
				// }
			}
			sequence++
		}
	})
}

// createInitialStreamingCard 创建初始显示“无相正在思考...”的卡片
func (l *LarkRobot) createInitialStreamingCard() (string, error) {
	cardData := map[string]interface{}{
		"schema": "2.0",
		"header": map[string]interface{}{
			"title":    map[string]interface{}{"content": "无相正在思考...", "tag": "plain_text"},
			"template": "blue",
		},
		"config": map[string]interface{}{
			"update_multi":   true,
			"streaming_mode": true,
		},
		"body": map[string]interface{}{
			"direction": "vertical",
			"elements": []interface{}{
				map[string]interface{}{
					"tag":        "markdown",
					"content":    "正在思考...",
					"element_id": "streaming_txt",
				},
			},
		},
	}
	return l.callCreateCard(cardData)
}

// transitionToAnswerPhase 思考结束后，将内容放入折叠面板，并准备 answer_txt
func (l *LarkRobot) transitionToAnswerPhase(cardID string, finalThinking string, sequence int) {
	headerData := map[string]interface{}{
		"title":    map[string]interface{}{"content": "无相已完成思考", "tag": "plain_text"},
		"template": "blue",
	}
	bodyData := map[string]interface{}{
		"direction": "vertical",
		"elements": []interface{}{
			map[string]interface{}{
				"tag": "collapsible_panel",
				"header": map[string]interface{}{
					"title": map[string]interface{}{"tag": "plain_text", "content": "点击查看思考过程"},
				},
				"elements": []interface{}{
					map[string]interface{}{"tag": "markdown", "content": finalThinking},
				},
			},
			map[string]interface{}{
				"tag":        "markdown",
				"content":    "正在回答...",
				"element_id": "answer_txt",
			},
		},
	}

	// 构建完整的更新请求体，必须包含 schema: 2.0 提升兼容性
	updateBody := map[string]interface{}{
		"schema": "2.0",
		"header": headerData,
		"body":   bodyData,
		"config": map[string]interface{}{
			"update_multi":   true,
			"streaming_mode": true,
		},
	}
	updateJSON, _ := json.Marshal(updateBody)

	l.Client.Cardkit.V1.Card.Update(l.Robot.Ctx, larkcardkit.NewUpdateCardReqBuilder().
		CardId(cardID).
		Body(larkcardkit.NewUpdateCardReqBodyBuilder().
			Card(larkcardkit.NewCardBuilder().
				Type("card_json").
				Data(string(updateJSON)).
				Build()).
			Sequence(sequence).
			Build()).
		Build())
}

// finishResponse 最终完成
func (l *LarkRobot) finishResponse(cardID string, finalAnswer string, sequence int) {
	// 1. 先推送最后一段内容
	l.updateStreamingElement(cardID, "answer_txt", finalAnswer, sequence)

	// 2. 更新标题为完成
	headerData := map[string]interface{}{
		"title":    map[string]interface{}{"content": "无相回复完成", "tag": "plain_text"},
		"template": "green",
	}

	// 构建更新请求体
	updateBody := map[string]interface{}{
		"schema": "2.0",
		"header": headerData,
		"config": map[string]interface{}{
			"update_multi":   true,
			"streaming_mode": false, // 结束后关闭流式模式
		},
	}
	updateJSON, _ := json.Marshal(updateBody)

	l.Client.Cardkit.V1.Card.Update(l.Robot.Ctx, larkcardkit.NewUpdateCardReqBuilder().
		CardId(cardID).
		Body(larkcardkit.NewUpdateCardReqBodyBuilder().
			Card(larkcardkit.NewCardBuilder().
				Type("card_json").
				Data(string(updateJSON)).
				Build()).
			Sequence(sequence+1).
			Build()).
		Build())
}

func (l *LarkRobot) updateStreamingElement(cardID, elementID, content string, sequence int) {
	if content == "" {
		content = "..."
	}
	l.Client.Cardkit.V1.CardElement.Content(l.Robot.Ctx, larkcardkit.NewContentCardElementReqBuilder().
		CardId(cardID).
		ElementId(elementID).
		Body(larkcardkit.NewContentCardElementReqBodyBuilder().
			Content(content).
			Sequence(sequence).
			Build()).
		Build())
}

func (l *LarkRobot) callCreateCard(cardData map[string]interface{}) (string, error) {
	cardJSON, _ := json.Marshal(cardData)
	resp, err := l.Client.Cardkit.V1.Card.Create(l.Robot.Ctx, larkcardkit.NewCreateCardReqBuilder().
		Body(larkcardkit.NewCreateCardReqBodyBuilder().
			Type("card_json").
			Data(string(cardJSON)).
			Build()).
		Build())
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", fmt.Errorf("code: %d, msg: %s", resp.Code, resp.Msg)
	}
	return *resp.Data.CardId, nil
}

func (l *LarkRobot) replyWithCardID(cardID string) error {
	_, messageId, _ := l.Robot.GetChatIdAndMsgIdAndUserID()
	content := map[string]interface{}{
		"type": "card",
		"data": map[string]interface{}{"card_id": cardID},
	}
	cJSON, _ := json.Marshal(content)
	_, err := l.Client.Im.Message.Reply(l.Robot.Ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(messageId).
		Body(larkim.NewReplyMessageReqBodyBuilder().
			MsgType(larkim.MsgTypeInteractive).
			Content(string(cJSON)).
			Build()).
		Build())
	return err
}

func (l *LarkRobot) updateLarkCard(larkMsgId string, content string, isFinished bool) string {
	return larkMsgId
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

func (l *LarkRobot) sendImg() {
	l.Robot.TalkingPreCheck(func() {
		chatId, msgId, _ := l.Robot.GetChatIdAndMsgIdAndUserID()
		prompt := strings.TrimSpace(l.Prompt)
		if prompt == "" {
			l.Robot.SendMsg(chatId, i18n.GetMessage("photo_empty_content", nil), msgId, tgbotapi.ModeMarkdown, nil)
			return
		}
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
			l.Robot.SendMsg(chatId, err.Error(), msgId, tgbotapi.ModeMarkdown, nil)
			return
		}
		err = l.sendMedia(imageContent, utils.DetectImageFormat(imageContent), "image")
		if err != nil {
			l.Robot.SendMsg(chatId, err.Error(), msgId, tgbotapi.ModeMarkdown, nil)
			return
		}
		l.Robot.saveRecord(imageContent, lastImageContent, param.ImageRecordType, totalToken)
	})
}

func (l *LarkRobot) sendMedia(media []byte, contentType, sType string) error {
	postContent := make([]larkim.MessagePostElement, 0)
	chatId, _, _ := l.Robot.GetChatIdAndMsgIdAndUserID()
	if sType == "image" {
		imageKey, err := l.getImageInfo(media)
		if err != nil {
			return err
		}
		postContent = append(postContent, &larkim.MessagePostImage{ImageKey: imageKey})
	} else {
		fileKey, err := l.getVideoInfo(media)
		if err != nil {
			return err
		}
		postContent = append(postContent, &larkim.MessagePostMedia{FileKey: fileKey})
	}
	msgContent, _ := larkim.NewMessagePost().ZhCn(larkim.NewMessagePostContent().AppendContent(postContent).Build()).Build()
	res, err := l.Client.Im.Message.Create(l.Robot.Ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.ReceiveIdTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().MsgType(larkim.MsgTypePost).ReceiveId(chatId).Content(msgContent).Build()).
		Build())
	if err != nil || !res.Success() {
		return errors.New("send message fail")
	}
	return nil
}

func (l *LarkRobot) sendVideo() {
	l.Robot.TalkingPreCheck(func() {
		chatId, msgId, _ := l.Robot.GetChatIdAndMsgIdAndUserID()
		prompt := strings.TrimSpace(l.Prompt)
		if prompt == "" {
			l.Robot.SendMsg(chatId, i18n.GetMessage("video_empty_content", nil), msgId, tgbotapi.ModeMarkdown, nil)
			return
		}
		videoContent, totalToken, err := l.Robot.CreateVideo(prompt, l.ImageContent)
		if err != nil {
			l.Robot.SendMsg(chatId, err.Error(), msgId, tgbotapi.ModeMarkdown, nil)
			return
		}
		err = l.sendMedia(videoContent, utils.DetectVideoMimeType(videoContent), "video")
		if err != nil {
			l.Robot.SendMsg(chatId, err.Error(), msgId, tgbotapi.ModeMarkdown, nil)
			return
		}
		l.Robot.saveRecord(videoContent, l.ImageContent, param.VideoRecordType, totalToken)
	})
}

func (l *LarkRobot) executeChain() {
	messageChan := &MsgChan{NormalMessageChan: make(chan *param.MsgInfo)}
	go l.Robot.ExecChain(l.Prompt, messageChan)
	go l.Robot.HandleUpdate(messageChan, "opus")
}

func (l *LarkRobot) sendTextStream(messageChan *MsgChan) {
	var msg *param.MsgInfo
	for msg = range messageChan.NormalMessageChan {
		l.updateLarkCard(msg.MsgId, msg.Content, msg.Finished)
	}
}

func GetInteractiveCard(content string, isFinished bool) string {
	template := "blue"
	statusText := "无相正在思考..."
	if isFinished {
		template = "green"
		statusText = "无相回复完成"
	}
	card := map[string]interface{}{
		"schema": "2.0",
		"header": map[string]interface{}{
			"title":    map[string]interface{}{"content": statusText, "tag": "plain_text"},
			"template": template,
		},
		"elements": []interface{}{
			map[string]interface{}{
				"tag":  "div",
				"text": map[string]interface{}{"content": content, "tag": "lark_md"},
			},
			map[string]interface{}{"tag": "hr"},
			map[string]interface{}{
				"tag": "div",
				"text": map[string]interface{}{
					"tag":     "plain_text",
					"content": "由青藤云安全提供技术支持",
				},
			},
		},
	}
	b, _ := json.Marshal(card)
	return string(b)
}

func (l *LarkRobot) executeLLM() {
	messageChan := &MsgChan{NormalMessageChan: make(chan *param.MsgInfo)}
	go l.Robot.HandleUpdate(messageChan, "opus")
	go l.Robot.ExecLLM(l.Prompt, messageChan)
}

func (l *LarkRobot) GetMessageContent() (bool, error) {
	if l.Message == nil || l.Message.Event == nil || l.Message.Event.Message == nil {
		return false, errors.New("nil message event")
	}
	_, msgId, _ := l.Robot.GetChatIdAndMsgIdAndUserID()
	msgType := larkcore.StringValue(l.Message.Event.Message.MessageType)
	botShowName := ""
	if msgType == larkim.MsgTypeText {
		textMsg := new(MessageText)
		json.Unmarshal([]byte(larkcore.StringValue(l.Message.Event.Message.Content)), textMsg)
		l.Command, l.Prompt = ParseCommand(textMsg.Text)
		for _, at := range l.Message.Event.Message.Mentions {
			if larkcore.StringValue(at.Name) == l.BotName {
				botShowName = larkcore.StringValue(at.Key)
				break
			}
		}
		l.Prompt = strings.ReplaceAll(l.Prompt, "@"+botShowName, "")
	} else if msgType == larkim.MsgTypePost {
		postMsg := new(MessagePostContent)
		json.Unmarshal([]byte(larkcore.StringValue(l.Message.Event.Message.Content)), postMsg)
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
						larkim.NewGetMessageResourceReqBuilder().MessageId(msgId).FileKey(msgPostContent.ImageKey).Type("image").Build())
					if err == nil && resp.Success() {
						l.ImageContent, _ = io.ReadAll(resp.File)
					}
				case "at":
					if l.BotName == msgPostContent.UserName {
						botShowName = msgPostContent.UserName
					}
				}
			}
		}
	} else if msgType == larkim.MsgTypeAudio {
		msgAudio := new(larkim.MessageAudio)
		json.Unmarshal([]byte(larkcore.StringValue(l.Message.Event.Message.Content)), msgAudio)
		resp, err := l.Client.Im.V1.MessageResource.Get(l.Robot.Ctx,
			larkim.NewGetMessageResourceReqBuilder().MessageId(msgId).FileKey(msgAudio.FileKey).Type("file").Build())
		if err == nil && resp.Success() {
			l.AudioContent, _ = io.ReadAll(resp.File)
			l.Prompt, _ = l.Robot.GetAudioContent(l.AudioContent)
		}
	} else if msgType == larkim.MsgTypeImage {
		msgImage := new(larkim.MessageImage)
		json.Unmarshal([]byte(larkcore.StringValue(l.Message.Event.Message.Content)), msgImage)
		resp, err := l.Client.Im.V1.MessageResource.Get(l.Robot.Ctx,
			larkim.NewGetMessageResourceReqBuilder().MessageId(msgId).FileKey(msgImage.ImageKey).Type("image").Build())
		if err == nil && resp.Success() {
			l.ImageContent, _ = io.ReadAll(resp.File)
		}
	}
	l.Prompt = strings.ReplaceAll(l.Prompt, "@"+l.BotName, "")
	return botShowName == l.BotName || larkcore.StringValue(l.Message.Event.Message.ChatType) == "p2p", nil
}

func (l *LarkRobot) getPrompt() string { return l.Prompt }
func (l *LarkRobot) getPerMsgLen() int { return 4500 }

func (l *LarkRobot) sendVoiceContent(voiceContent []byte, duration int) error {
	_, messageId, _ := l.Robot.GetChatIdAndMsgIdAndUserID()
	resp, err := l.Client.Im.V1.File.Create(l.Robot.Ctx, larkim.NewCreateFileReqBuilder().
		Body(larkim.NewCreateFileReqBodyBuilder().FileType("opus").FileName(utils.RandomFilename(".ogg")).Duration(duration).File(bytes.NewReader(voiceContent)).Build()).Build())
	if err != nil || !resp.Success() {
		return errors.New("upload voice fail")
	}
	audio := larkim.MessageAudio{FileKey: *resp.Data.FileKey}
	msgContent, _ := audio.String()
	updateRes, err := l.Client.Im.Message.Reply(l.Robot.Ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(messageId).Body(larkim.NewReplyMessageReqBodyBuilder().MsgType(larkim.MsgTypeAudio).Content(msgContent).Build()).Build())
	if err != nil || !updateRes.Success() {
		return errors.New("reply voice fail")
	}
	return nil
}

func (l *LarkRobot) setCommand(command string) { l.Command = command }
func (l *LarkRobot) getCommand() string        { return l.Command }
func (l *LarkRobot) getUserName() string       { return l.UserName }

func (l *LarkRobot) getVideoInfo(videoContent []byte) (string, error) {
	format := utils.DetectVideoMimeType(videoContent)
	resp, err := l.Client.Im.V1.File.Create(l.Robot.Ctx, larkim.NewCreateFileReqBuilder().
		Body(larkim.NewCreateFileReqBodyBuilder().FileType(format).FileName(utils.RandomFilename(format)).Duration(conf.VideoConfInfo.Duration).File(bytes.NewReader(videoContent)).Build()).Build())
	if err != nil || !resp.Success() {
		return "", errors.New("upload video fail")
	}
	return larkcore.StringValue(resp.Data.FileKey), nil
}

func (l *LarkRobot) getImageInfo(imageContent []byte) (string, error) {
	resp, err := l.Client.Im.V1.Image.Create(l.Robot.Ctx, larkim.NewCreateImageReqBuilder().
		Body(larkim.NewCreateImageReqBodyBuilder().ImageType("message").Image(bytes.NewReader(imageContent)).Build()).Build())
	if err != nil || !resp.Success() {
		return "", errors.New("upload image fail")
	}
	return larkcore.StringValue(resp.Data.ImageKey), nil
}

func (l *LarkRobot) setPrompt(prompt string) { l.Prompt = prompt }
func (l *LarkRobot) getAudio() []byte        { return l.AudioContent }
func (l *LarkRobot) getImage() []byte        { return l.ImageContent }
func (l *LarkRobot) setImage(image []byte)   { l.ImageContent = image }

type MessagePostMarkdown struct {
	Text string `json:"text,omitempty"`
}

func (m *MessagePostMarkdown) Tag() string { return "md" }
func (m *MessagePostMarkdown) IsPost()     {}
func (m *MessagePostMarkdown) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]interface{}{"tag": "md", "text": m.Text})
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
