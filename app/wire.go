package app

import (
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wireWriteTimeout   = 10 * time.Second
	wirePongTimeout    = 60 * time.Second
	wirePingPeriod     = (wirePongTimeout * 9) / 10
	wireMaxMessageSize = 512
)

type wireMessageType int

const (
	wmtReady wireMessageType = iota
	wmtPlayerRegistered
	wmtPlayerUnregistered
	wmtGameStarted
	wmtNextQuestion
	wmtTask
	wmtTimer
	wmtAnswer
	wmtTaskFinished
	wmtGameFinished

	wmtNotReady     = -1
	wmtPlayerExists = -2
)

type wireMessage struct {
	Type wireMessageType `json:"type"`
	Data interface{}     `json:"data,omitempty"`
}

var wireUpgrader = websocket.Upgrader{
	ReadBufferSize:    1024,
	WriteBufferSize:   1024,
	EnableCompression: true,
}

func wireReader(pool *Pool, player *Player) {
	defer func() {
		pool.unregister <- player
		_ = player.ws.Close()
	}()

	player.ws.SetReadLimit(wireMaxMessageSize)
	_ = player.ws.SetReadDeadline(time.Now().Add(wirePongTimeout))
	player.ws.SetPongHandler(func(string) error {
		_ = player.ws.SetReadDeadline(time.Now().Add(wirePongTimeout))
		return nil
	})

	for {
		var wm *wireMessage
		if err := player.ws.ReadJSON(&wm); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("error: %v", err)
			}
			break
		}

		if player.IsAuthor {
			switch wm.Type {
			case wmtGameStarted:
				numTasks := player.gameplay.Start()
				pool.broadcast <- &broadcastMessage{
					Game: player.Game,
					Message: &wireMessage{
						Type: wmtGameStarted,
						Data: map[string]interface{}{
							"num_tasks": numTasks,
						},
					},
				}
			case wmtNextQuestion:
				var task Task
				task = player.gameplay.NextTask(func(timer int) {
					pool.broadcast <- &broadcastMessage{
						Game: player.Game,
						Message: &wireMessage{
							Type: wmtTimer,
							Data: timer,
						},
					}
				}, func(gp *gameplay) {
					pool.broadcast <- &broadcastMessage{
						Game: player.Game,
						Message: &wireMessage{
							Type: wmtTaskFinished,
							Data: map[string]interface{}{
								"correct_answer_idx": task.correctAnswerIdx(),
								"answer_stats":       gp.currentAnswerStats,
								"scores":             gp.scores.Leaderboard(),
							},
						},
					}
				})
				if task != nil {
					pool.broadcast <- &broadcastMessage{
						Game: player.Game,
						Message: &wireMessage{
							Type: wmtTask,
							Data: map[string]interface{}{
								"index":          player.gameplay.currentTaskIdx + 1,
								"question":       task.question(),
								"answers":        task.answers(),
								"time_to_answer": task.timeToAnswer() / time.Second,
							},
						},
					}
				}
			case wmtGameFinished:
				scores := player.gameplay.Finish()
				pool.broadcast <- &broadcastMessage{
					Game: player.Game,
					Message: &wireMessage{
						Type: wmtGameFinished,
						Data: scores.Leaderboard(),
					},
				}
			}
		}

		switch wm.Type {
		case wmtAnswer:
			if idx, ok := wm.Data.(float64); ok {
				score := player.gameplay.Answer(player, int(idx))
				if score >= 0 {
					player.send <- &wireMessage{
						Type: wmtAnswer,
						Data: score,
					}
				}
			}
		}
	}
}

func wireWriter(_ *Pool, player *Player) {
	ticker := time.NewTicker(wirePingPeriod)
	defer func() {
		ticker.Stop()
		_ = player.ws.Close()
	}()

	for {
		select {
		case wm, ok := <-player.send:
			_ = player.ws.SetWriteDeadline(time.Now().Add(wireWriteTimeout))
			if !ok {
				_ = player.ws.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := player.ws.WriteJSON(wm); err != nil {
				return
			}
		case <-ticker.C:
			_ = player.ws.SetWriteDeadline(time.Now().Add(wireWriteTimeout))
			if err := player.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func WireHandler(pool *Pool, player *Player, w http.ResponseWriter, r *http.Request) {
	ws, err := wireUpgrader.Upgrade(w, r, nil)
	if err != nil {
		panic(err)
	}

	player.ws = ws
	player.send = make(chan *wireMessage, 1)
	pool.register <- player

	go wireReader(pool, player)
	go wireWriter(pool, player)
}
