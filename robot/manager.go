package robot

import (
	"context"
	"sync"

	"github.com/yincongcyincong/MuseBot/db"
	"github.com/yincongcyincong/MuseBot/logger"
)

type BotInstance struct {
	Config *db.Bot
	Cancel context.CancelFunc
}

type BotManager struct {
	instances sync.Map // map[int64]*BotInstance
	ctx       context.Context
	cancel    context.CancelFunc
}

var GlobalBotManager *BotManager

func InitBotManager() {
	ctx, cancel := context.WithCancel(context.Background())
	GlobalBotManager = &BotManager{
		ctx:    ctx,
		cancel: cancel,
	}
}

func (m *BotManager) StartBot(botConfig *db.Bot) {
	// 防止重复启动
	if _, ok := m.instances.Load(botConfig.ID); ok {
		logger.Info("Bot already running", "id", botConfig.ID, "name", botConfig.Name)
		return
	}

	botCtx, botCancel := context.WithCancel(m.ctx)
	instance := &BotInstance{
		Config: botConfig,
		Cancel: botCancel,
	}

	m.instances.Store(botConfig.ID, instance)

	go func() {
		logger.Info("Starting bot", "id", botConfig.ID, "name", botConfig.Name, "type", botConfig.Type)
		switch botConfig.Type {
		case "lark":
			// 传入 ID, Secret 启动
			RunLarkRobot(botCtx, botConfig.AppID, botConfig.AppSecret)
		default:
			logger.Error("Unsupported bot type", "type", botConfig.Type)
		}
		
		// 运行结束（如 context 取消或崩溃）后清理
		m.instances.Delete(botConfig.ID)
		db.UpdateBotStatus(botConfig.ID, 0)
	}()

	// 更新数据库状态为运行中
	db.UpdateBotStatus(botConfig.ID, 1)
}

func (m *BotManager) StopBot(id int64) {
	if val, ok := m.instances.Load(id); ok {
		instance := val.(*BotInstance)
		instance.Cancel() // 通过 context 通知机器人停止
		m.instances.Delete(id)
		db.UpdateBotStatus(id, 0)
		logger.Info("Bot stopped", "id", id)
	}
}

func (m *BotManager) LoadAllBots() {
	bots, err := db.GetBots()
	if err != nil {
		logger.Error("Load bots from db fail", "err", err)
		return
	}

	for _, bot := range bots {
		if bot.Status == 1 {
			m.StartBot(bot)
		}
	}
}
