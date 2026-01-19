package robot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"runtime/debug"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkapplication "github.com/larksuite/oapi-sdk-go/v3/service/application/v6"
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

// 兼容旧代码的全局变量（通过函数包装或直接导出）
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

	// 获取机器人名称
	nameResp, err := instance.Client.Application.Application.Get(ctx, larkapplication.NewGetApplicationReqBuilder().
		AppId(appID).Lang("zh_cn").Build())
	if err == nil && nameResp.Success() {
		instance.BotName = larkcore.StringValue(nameResp.Data.App.AppName)
	} else {
		instance.BotName = "LarkBot"
		logger.ErrorCtx(ctx, "get robot name error", "error", err, "resp", nameResp)
	}

	// 如果是默认配置启动的，设置给全局变量（兼容旧代码引用）
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

				// 健壮获取用户信息
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

	// group need to at bot
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
	return l.Command
}

func (l *LarkRobot) requestLLM(content string) {
	if !strings.Contains(content, "/") && !strings.Contains(content, "$") && l.Prompt == "" {
		l.Prompt = content
	}
	l.Robot.ExecCmd(content, l.sendChatMessage, nil, nil)
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
		postContent = append(postContent, &larkim.MessagePostImage{
			ImageKey: imageKey,
		})
	} else {
		fileKey, err := l.getVideoInfo(media)
		if err != nil {
			return err
		}
		postContent = append(postContent, &larkim.MessagePostMedia{
			FileKey: fileKey,
		})
	}

	msgContent, _ := larkim.NewMessagePost().ZhCn(larkim.NewMessagePostContent().AppendContent(postContent).Build()).Build()
	res, err := l.Client.Im.Message.Create(l.Robot.Ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.ReceiveIdTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			MsgType(larkim.MsgTypePost).
			ReceiveId(chatId).
			Content(msgContent).
			Build()).
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

func (l *LarkRobot) sendTextStream(messageChan *MsgChan) {
	var msg *param.MsgInfo
	chatId, messageId, _ := l.Robot.GetChatIdAndMsgIdAndUserID()
	for msg = range messageChan.NormalMessageChan {
		if len(msg.Content) == 0 {
			msg.Content = "get nothing from llm!"
		}

		if msg.MsgId == "" {
			msgId := l.Robot.SendMsg(chatId, msg.Content, messageId, tgbotapi.ModeMarkdown, nil)
			msg.MsgId = msgId
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

func (l *LarkRobot) GetMessageContent() (bool, error) {
	if l.Message == nil || l.Message.Event == nil || l.Message.Event.Message == nil {
		return false, errors.New("nil message event")
	}

	_, msgId, _ := l.Robot.GetChatIdAndMsgIdAndUserID()
	msgType := larkcore.StringValue(l.Message.Event.Message.MessageType)
	botShowName := ""

	if msgType == larkim.MsgTypeText {
		textMsg := new(MessageText)
		err := json.Unmarshal([]byte(larkcore.StringValue(l.Message.Event.Message.Content)), textMsg)
		if err != nil {
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
	} else if msgType == larkim.MsgTypePost {
		postMsg := new(MessagePostContent)
		err := json.Unmarshal([]byte(larkcore.StringValue(l.Message.Event.Message.Content)), postMsg)
		if err != nil {
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
		Body(larkim.NewCreateFileReqBodyBuilder().
			FileType("opus").FileName(utils.RandomFilename(".ogg")).
			Duration(duration).File(bytes.NewReader(voiceContent)).Build()).Build())
	if err != nil || !resp.Success() {
		return errors.New("upload voice fail")
	}

	audio := larkim.MessageAudio{FileKey: *resp.Data.FileKey}
	msgContent, _ := audio.String()
	updateRes, err := l.Client.Im.Message.Reply(l.Robot.Ctx, larkim.NewReplyMessageReqBuilder().
		MessageId(messageId).Body(larkim.NewReplyMessageReqBodyBuilder().
		MsgType(larkim.MsgTypeAudio).Content(msgContent).Build()).Build())
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
		Body(larkim.NewCreateFileReqBodyBuilder().
			FileType(format).FileName(utils.RandomFilename(format)).
			Duration(conf.VideoConfInfo.Duration).File(bytes.NewReader(videoContent)).Build()).Build())
	if err != nil || !resp.Success() {
		return "", errors.New("upload video fail")
	}
	return larkcore.StringValue(resp.Data.FileKey), nil
}

func (l *LarkRobot) getImageInfo(imageContent []byte) (string, error) {
	resp, err := l.Client.Im.V1.Image.Create(l.Robot.Ctx, larkim.NewCreateImageReqBuilder().
		Body(larkim.NewCreateImageReqBodyBuilder().ImageType("message").
			Image(bytes.NewReader(imageContent)).Build()).Build())
	if err != nil || !resp.Success() {
		return "", errors.New("upload image fail")
	}
	return larkcore.StringValue(resp.Data.ImageKey), nil
}

func (l *LarkRobot) setPrompt(prompt string) { l.Prompt = prompt }
func (l *LarkRobot) getAudio() []byte        { return l.AudioContent }
func (l *LarkRobot) getImage() []byte        { return l.ImageContent }
func (l *LarkRobot) setImage(image []byte)   { l.ImageContent = image }
