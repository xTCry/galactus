package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	socketio "github.com/googollee/go-socket.io"
	"github.com/gorilla/mux"
)

const ConnectCodeLength = 8
const SecretKeyLength = 16

var ctx = context.Background()

func activeGamesCode() string {
	return "automuteus:games"
}

func refreshConnectCodeLiveness(client *redis.Client, code string) {
	t := time.Now().Unix()
	client.ZAdd(ctx, activeGamesCode(), &redis.Z{
		Score:  float64(t),
		Member: code,
	})
}

type Broker struct {
	client *redis.Client

	//map of socket IDs to connection codes
	connections     map[string]string
	ackKillChannels map[string]chan bool
	connectionsLock sync.RWMutex
}

func NewBroker(redisAddr, redisUser, redisPass string) *Broker {
	rdb := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Username: redisUser,
		Password: redisPass,
		DB:       0, // use default DB
	})
	return &Broker{
		client:          rdb,
		connections:     map[string]string{},
		ackKillChannels: map[string]chan bool{},
		connectionsLock: sync.RWMutex{},
	}
}

func (broker *Broker) Start(port string) {
	server, err := socketio.NewServer(nil)
	if err != nil {
		log.Fatal(err)
	}
	server.OnConnect("/", func(s socketio.Conn) error {
		s.SetContext("")
		log.Println("connected:", s.ID())
		return nil
	})

	// => Plugin zone
	// Secret key for connecting the Impostor server plugin
	server.OnEvent("/", "secretKey", func(s socketio.Conn, msg string) {
		log.Printf("Received secretKey: \"%s\"", msg)

		if len(msg) != SecretKeyLength {
			s.Close()
		} else {
			killChannel := make(chan bool)

			broker.connectionsLock.Lock()
			broker.connections[s.ID()] = msg
			broker.ackKillChannels[s.ID()] = killChannel
			broker.connectionsLock.Unlock()

			// err := PushJob(ctx, broker.client, msg, Connection, "true")
			// if err != nil {
			// 	log.Println(err)
			// }
			// go broker.AckWorker(ctx, msg, killChannel)
		}
	})
	server.OnEvent("/", "newGame", func(s socketio.Conn, msg string) {
		log.Println("newGame:", msg)

		broker.connectionsLock.RLock()
		if secretKey, ok := broker.connections[s.ID()]; ok {
			err := PushJob(ctx, broker.client, secretKey, NewGame, msg)
			if err != nil {
				log.Println(err)
			} else {
				s.Emit("gameAdded", msg)
			}
		}
		broker.connectionsLock.RUnlock()
	})
	server.OnEvent("/", "endGame", func(s socketio.Conn, msg string) {
		log.Println("endGame:", msg)

		broker.connectionsLock.RLock()
		if secretKey, ok := broker.connections[s.ID()]; ok {
			err := PushJob(ctx, broker.client, secretKey, EndGame, msg)
			if err != nil {
				log.Println(err)
			}
		}
		broker.connectionsLock.RUnlock()
	})
	// <= end Plugin zone

	server.OnEvent("/", "connectCode", func(s socketio.Conn, msg string) {
		log.Printf("Received connection code: \"%s\"", msg)

		if len(msg) != ConnectCodeLength {
			s.Close()
		} else {
			killChannel := make(chan bool)

			broker.connectionsLock.Lock()
			broker.connections[s.ID()] = msg
			broker.ackKillChannels[s.ID()] = killChannel
			broker.connectionsLock.Unlock()

			refreshConnectCodeLiveness(broker.client, msg)

			err := PushJob(ctx, broker.client, msg, Connection, "true")
			if err != nil {
				log.Println(err)
			}
			go broker.AckWorker(ctx, msg, killChannel)
		}
	})
	server.OnEvent("/", "lobby", func(s socketio.Conn, msg string) {
		log.Println("lobby:", msg)
		//TODO validation

		broker.connectionsLock.RLock()
		if cCode, ok := broker.connections[s.ID()]; ok {
			refreshConnectCodeLiveness(broker.client, cCode)

			err := PushJob(ctx, broker.client, cCode, Lobby, msg)
			if err != nil {
				log.Println(err)
			}
		}
		broker.connectionsLock.RUnlock()
	})
	server.OnEvent("/", "state", func(s socketio.Conn, msg string) {
		log.Println("phase received from capture: ", msg)
		_, err := strconv.Atoi(msg)
		if err != nil {
			log.Println(err)
		} else {
			broker.connectionsLock.RLock()
			if cCode, ok := broker.connections[s.ID()]; ok {
				refreshConnectCodeLiveness(broker.client, cCode)

				err := PushJob(ctx, broker.client, cCode, State, msg)
				if err != nil {
					log.Println(err)
				}
			}
			broker.connectionsLock.RUnlock()
		}
	})
	server.OnEvent("/", "player", func(s socketio.Conn, msg string) {
		log.Println("player received from capture: ", msg)

		broker.connectionsLock.RLock()
		if cCode, ok := broker.connections[s.ID()]; ok {
			refreshConnectCodeLiveness(broker.client, cCode)

			err := PushJob(ctx, broker.client, cCode, Player, msg)
			if err != nil {
				log.Println(err)
			}

		}
		broker.connectionsLock.RUnlock()
	})
	server.OnError("/", func(s socketio.Conn, e error) {
		log.Println("meet error:", e)
	})
	server.OnDisconnect("/", func(s socketio.Conn, reason string) {
		log.Println("Client connection closed: ", reason)

		broker.connectionsLock.RLock()
		if cCode, ok := broker.connections[s.ID()]; ok {
			err := PushJob(ctx, broker.client, cCode, Connection, "false")
			if err != nil {
				log.Println(err)
			}
		}
		broker.connectionsLock.RUnlock()

		broker.connectionsLock.Lock()
		if c, ok := broker.ackKillChannels[s.ID()]; ok {
			c <- true
		}
		delete(broker.ackKillChannels, s.ID())
		delete(broker.connections, s.ID())
		broker.connectionsLock.Unlock()
	})

	go server.Serve()
	defer server.Close()

	router := mux.NewRouter()
	router.Handle("/socket.io/", server)
	router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		broker.connectionsLock.RLock()
		activeConns := len(broker.connections)
		broker.connectionsLock.RUnlock()

		activeGames := GetActiveGames(broker.client)
		version := GetVersion(broker.client)
		totalGuilds := GetGuildCounter(broker.client, version)

		data := map[string]interface{}{
			"version":           version,
			"totalGuilds":       totalGuilds,
			"activeConnections": activeConns,
			"activeGames":       activeGames,
		}

		jsonBytes, err := json.Marshal(data)
		if err != nil {
			log.Println(err)
		}
		w.Write(jsonBytes)
	})
	router.HandleFunc("/debug", func(w http.ResponseWriter, r *http.Request) {
		broker.connectionsLock.RLock()
		data := map[string]interface{}{
			"activeConnections": len(broker.connections),
			"connections":       broker.connections,
		}
		broker.connectionsLock.RUnlock()

		jsonBytes, err := json.Marshal(data)
		if err != nil {
			log.Println(err)
		}
		w.Write(jsonBytes)
	})

	log.Printf("Message broker is running on port %s...\n", port)
	log.Fatal(http.ListenAndServe(":"+port, router))
}

func totalGuildsKey(version string) string {
	return "automuteus:count:guilds:version-" + version
}

func versionKey() string {
	return "automuteus:version"
}

func GetVersion(client *redis.Client) string {
	v, err := client.Get(ctx, versionKey()).Result()
	if err != nil {
		log.Println(err)
	}
	return v
}

func GetGuildCounter(client *redis.Client, version string) int64 {
	count, err := client.SCard(ctx, totalGuildsKey(version)).Result()
	if err != nil {
		log.Println(err)
		return 0
	}
	return count
}

func GetActiveGames(client *redis.Client) int64 {
	now := time.Now()
	before := now.Add(-(time.Second * 600))
	count, err := client.ZCount(ctx, activeGamesCode(), fmt.Sprintf("%d", before.Unix()), fmt.Sprintf("%d", now.Unix())).Result()
	if err != nil {
		log.Println(err)
		return 0
	}
	return count
}

//anytime a bot "acks", then push a notification
func (broker *Broker) AckWorker(ctx context.Context, connCode string, killChan <-chan bool) {
	pubsub := AckSubscribe(ctx, broker.client, connCode)

	for {
		select {
		case <-killChan:
			pubsub.Close()
			return
		case <-pubsub.Channel():
			err := PushJob(ctx, broker.client, connCode, Connection, "true")
			if err != nil {
				log.Println(err)
			}
			notify(ctx, broker.client, connCode)
			break
		}
	}
}
