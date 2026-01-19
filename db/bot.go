package db

import (
	"database/sql"
	"time"
)

type Bot struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"` // lark, telegram, etc.
	AppID      string `json:"app_id"`
	AppSecret  string `json:"app_secret"`
	Token      string `json:"token"`
	Status     int    `json:"status"` // 0:stopped 1:running
	CreateTime int64  `json:"create_time"`
	UpdateTime int64  `json:"update_time"`
	IsDeleted  int    `json:"is_deleted"`
}

func InsertBot(bot *Bot) (int64, error) {
	now := time.Now().Unix()
	insertSQL := `INSERT INTO bots (name, type, app_id, app_secret, token, status, create_time, update_time, is_deleted) 
                  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	result, err := DB.Exec(insertSQL, bot.Name, bot.Type, bot.AppID, bot.AppSecret, bot.Token, bot.Status, now, now, 0)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func GetBots() ([]*Bot, error) {
	querySQL := `SELECT id, name, type, app_id, app_secret, token, status, create_time, update_time FROM bots WHERE is_deleted = 0`
	rows, err := DB.Query(querySQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bots []*Bot
	for rows.Next() {
		var bot Bot
		err := rows.Scan(&bot.ID, &bot.Name, &bot.Type, &bot.AppID, &bot.AppSecret, &bot.Token, &bot.Status, &bot.CreateTime, &bot.UpdateTime)
		if err != nil {
			return nil, err
		}
		bots = append(bots, &bot)
	}
	return bots, nil
}

func GetBotByID(id int64) (*Bot, error) {
	querySQL := `SELECT id, name, type, app_id, app_secret, token, status, create_time, update_time FROM bots WHERE id = ? AND is_deleted = 0`
	row := DB.QueryRow(querySQL, id)
	var bot Bot
	err := row.Scan(&bot.ID, &bot.Name, &bot.Type, &bot.AppID, &bot.AppSecret, &bot.Token, &bot.Status, &bot.CreateTime, &bot.UpdateTime)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &bot, nil
}

func UpdateBotStatus(id int64, status int) error {
	updateSQL := `UPDATE bots SET status = ?, update_time = ? WHERE id = ?`
	_, err := DB.Exec(updateSQL, status, time.Now().Unix(), id)
	return err
}

func DeleteBot(id int64) error {
	updateSQL := `UPDATE bots SET is_deleted = 1, update_time = ? WHERE id = ?`
	_, err := DB.Exec(updateSQL, time.Now().Unix(), id)
	return err
}
