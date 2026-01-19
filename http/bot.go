package http

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/yincongcyincong/MuseBot/db"
	"github.com/yincongcyincong/MuseBot/param"
	"github.com/yincongcyincong/MuseBot/robot"
	"github.com/yincongcyincong/MuseBot/utils"
)

func CreateBot(w http.ResponseWriter, r *http.Request) {
	var bot db.Bot
	if err := json.NewDecoder(r.Body).Decode(&bot); err != nil {
		utils.Failure(r.Context(), w, r, param.CodeParamError, "Invalid params", nil)
		return
	}

	id, err := db.InsertBot(&bot)
	if err != nil {
		utils.Failure(r.Context(), w, r, param.CodeDBWriteFail, err.Error(), nil)
		return
	}
	utils.Success(r.Context(), w, r, map[string]int64{"id": id})
}

func StartBot(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, _ := strconv.ParseInt(idStr, 10, 64)
	botConfig, err := db.GetBotByID(id)
	if err != nil || botConfig == nil {
		utils.Failure(r.Context(), w, r, param.CodeParamError, "Bot not found", nil)
		return
	}

	robot.GlobalBotManager.StartBot(botConfig)
	utils.Success(r.Context(), w, r, "Bot starting")
}

func StopBot(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, _ := strconv.ParseInt(idStr, 10, 64)
	robot.GlobalBotManager.StopBot(id)
	utils.Success(r.Context(), w, r, "Bot stopping")
}

func ListBots(w http.ResponseWriter, r *http.Request) {
	bots, err := db.GetBots()
	if err != nil {
		utils.Failure(r.Context(), w, r, param.CodeDBQueryFail, err.Error(), nil)
		return
	}
	utils.Success(r.Context(), w, r, bots)
}
