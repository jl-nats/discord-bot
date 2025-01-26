package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type ConnState int

const (
	gatewayURLApiURL = "https://discord.com/api/v10/gateway/bot"
	gatewayURLParams = "/?v=10&encoding=json"
)

const (
	Disconnected ConnState = iota
	StartConnecting
	WaitForHello
	WaitForReady
	Connected
	Resuming
)

func (c ConnState) String() string {
	return [...]string{"Disconnected", "StartConnecting", "WaitForHello", "WaitForReady", "Connected", "Resuming"}[c]
}

type GatewayResponse struct {
	URL string `json:"url"`
}

type GatewayEventPayload struct {
	Op             int             `json:"op"`
	Data           json.RawMessage `json:"d"`
	SequenceNumber *int            `json:"s,omitempty"`
	EventName      *string         `json:"t,omitempty"`
}

type IdentifyData struct {
	Token      string     `json:"token"`
	Properties Properties `json:"properties"`
	Presence   Presence   `json:"presence"`
	Intents    int        `json:"intents"`
}

type Properties struct {
	OS      string `json:"os"`
	Browser string `json:"browser"`
	Device  string `json:"device"`
}

type Presence struct {
	Status string `json:"status"`
	Since  int    `json:"since"`
	AFK    bool   `json:"afk"`
}

type Activity struct {
	Name string `json:"name"`
	Type int    `json:"type"`
}

type ReadyEventData struct {
	User             User   `json:"user"`
	SessionID        string `json:"session_id"`
	ResumeGatewayURL string `json:"resume_gateway_url"`
}

type HelloEventData struct {
	Interval int `json:"heartbeat_interval"`
}

type ResumeEventData struct {
	Token              string `json:"token"`
	SessionID          string `json:"session_id"`
	LastSequenceNumber int    `json:"seq"`
}

type User struct {
	Id            string  `json:"id"`
	Username      string  `json:"username"`
	Discriminator string  `json:"discriminator"`
	GlobalName    *string `json:"global_name,omitempty"`
	Avatar        *string `json:"avatar"`
	Bot           *bool   `json:"bot,omitempty"`
}

type GatewayConnection struct {
	Conn               *websocket.Conn
	ConnectionState    ConnState
	SessionID          *string
	LastSequenceNumber *int
	ResumeGatewayURL   string
	HeartbeatInterval  *int
	Wg                 sync.WaitGroup
	WriteChan          chan []byte
	ACKChan            chan struct{}
	ReadChan           chan GatewayEventPayload
	ErrChan            chan error
	Mut                sync.Mutex
	Ctx                context.Context
	Cancel             context.CancelFunc
}

type Disconnect struct {
	Reason       string
	CanReconnect bool
}

type Message struct {
	Id        string `json:"id"`
	ChannelID string `json:"channel_id"`
	Author    User   `json:"author"`
	Content   string `json:"content"`
	Type      int    `json:"type"`
}

func newGatewayConnection() *GatewayConnection {
	return &GatewayConnection{
		Conn:               nil,
		ConnectionState:    Disconnected,
		SessionID:          nil,
		LastSequenceNumber: nil,
		ResumeGatewayURL:   "",
		HeartbeatInterval:  nil,
		Mut:                sync.Mutex{},
		Ctx:                nil,
		Cancel:             nil,
		ErrChan:            make(chan error),
	}
}

func (g *GatewayConnection) close() error {
	log.Println("Closing connection.")
	if g.Cancel != nil {
		g.Cancel()
	}

	g.Wg.Wait()
	if g.Conn != nil {
		g.Conn.Close()
	}

	g.setState(Disconnected)
	return nil
}

func (g *GatewayConnection) setState(state ConnState) {
	g.Mut.Lock()
	defer g.Mut.Unlock()
	log.Println("Connection state: ", state.String())
	g.ConnectionState = state

}

func (g *GatewayConnection) sendResume() error {
	if g.ConnectionState != WaitForHello {
		return errors.New("cannot send resume while not waiting for hello")
	}
	token := os.Getenv("TOKEN")
	resumeData := ResumeEventData{
		Token:              token,
		SessionID:          *g.SessionID,
		LastSequenceNumber: *g.LastSequenceNumber,
	}
	marshalledResumeData, err := json.Marshal(resumeData)
	if err != nil {
		return err
	}
	resumeDataPayload := GatewayEventPayload{
		Op:   6,
		Data: marshalledResumeData,
	}
	p, err := json.Marshal(resumeDataPayload)
	if err != nil {
		return err
	}
	g.WriteChan <- p
	return nil
}

func (g *GatewayConnection) connect(url string, resume bool, disconnectChan chan bool, messageChan chan Message) error {
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		log.Println("Error connecting to gateway.", err)
		disconnectChan <- false
		return err
	}
	g.Conn = conn
	g.Wg = sync.WaitGroup{}
	g.ACKChan = make(chan struct{})
	g.ReadChan = make(chan GatewayEventPayload)
	g.WriteChan = make(chan []byte)
	g.Ctx, g.Cancel = context.WithCancel(context.Background())
	g.setState(StartConnecting)
	defer g.close()
	g.beginReader()
	g.beginWriter()

	g.setState(WaitForHello)
	for {
		select {
		case <-g.Ctx.Done():
			log.Println("CONNECTION END DUE TO CANCEL")
			return nil
		case err := <-g.ErrChan:
			log.Println("DISCONNECTING DUE TO ERROR: ", err)
			disconnectChan <- false

			return err
		case msg := <-g.ReadChan:
			switch msg.Op {
			case 7:
				log.Println("Received RECONNECT")
				disconnectChan <- true
				return nil
			case 10:
				if g.ConnectionState != WaitForHello {
					disconnectChan <- false
					log.Println("Received HELLO while not waiting for it.")
					return errors.New("received hello event while not waiting for it")
				}
				var hello HelloEventData
				err = json.Unmarshal(msg.Data, &hello)
				if err != nil {
					log.Println("Error unmarshalling hello event.", err)
					disconnectChan <- false
					return err
				}
				g.startHeartbeat(hello.Interval)
				if resume {
					err = g.sendResume()
					if err != nil {
						log.Println("Error sending resume.", err)
						disconnectChan <- false
						return err
					}
					g.setState(Resuming)

				} else {
					err = g.sendIdentify()
					if err != nil {
						log.Println("Error sending identify.", err)
						disconnectChan <- false
						return err
					}
				}

				g.setState(WaitForReady)
			case 11:
				log.Println("Received HEARTBEAT_ACK")
				g.ACKChan <- struct{}{}
			case 0:
				switch *msg.EventName {
				case "READY":
					if g.ConnectionState != WaitForReady {
						disconnectChan <- false
						return errors.New("received ready event while not waiting for it")
					}
					var ready ReadyEventData
					err = json.Unmarshal(msg.Data, &ready)
					if err != nil {
						disconnectChan <- false
						return err
					}
					g.SessionID = &ready.SessionID
					g.ResumeGatewayURL = ready.ResumeGatewayURL
					log.Println("Ready event received. Logged in as: " + ready.User.Username + "#" + ready.User.Discriminator)
					g.setState(Connected)
				case "MESSAGE_CREATE":
					var message Message
					err = json.Unmarshal(msg.Data, &message)
					if err != nil {
						disconnectChan <- false
						return err
					}
					messageChan <- message
				}
			case 9:
				log.Println("Received INVALID_SESSION")
				var canReconnect bool
				err := json.Unmarshal(msg.Data, &canReconnect)
				if err != nil {
					disconnectChan <- false
					return err
				}
				disconnectChan <- canReconnect
				return nil

			}
		}
	}
}

func (g *GatewayConnection) beginReader() {
	g.Wg.Add(1)
	go func() {
		defer g.Wg.Done()
		readChan := make(chan readResult)

		for {

			go func() {
				_, msg, err := g.Conn.ReadMessage()
				readChan <- readResult{msg, err}
			}()

			select {
			case <-g.Ctx.Done():
				log.Println("Reader stopped.")
				return
			case result := <-readChan:
				if result.err != nil {
					select {
					case g.ErrChan <- result.err:
					default:
						return

					}
				}
				var payload GatewayEventPayload
				err := json.Unmarshal(result.msg, &payload)
				if err != nil {
					select {
					case g.ErrChan <- err:
					default:
						return

					}

				}
				g.Mut.Lock()
				if payload.SequenceNumber != nil {
					g.LastSequenceNumber = payload.SequenceNumber
				}
				g.Mut.Unlock()
				log.Println("Received | " + string(result.msg))
				select {
				case g.ReadChan <- payload:
				case <-g.Ctx.Done():
					log.Println("Reader stopped.")
					return
				}

			}

		}
	}()

}

type readResult struct {
	msg []byte
	err error
}

func (g *GatewayConnection) sendIdentify() error {
	log.Println("Sending identify.")
	if g.ConnectionState != WaitForHello {
		return errors.New("cannot send identify while not waiting for hello")
	}
	token := os.Getenv("TOKEN")
	identifyData := IdentifyData{
		Token: token,
		Properties: Properties{
			OS:      "linux",
			Browser: "discord-bot",
			Device:  "discord-bot",
		},
		Presence: Presence{
			Status: "online",
			Since:  0,
			AFK:    false,
		},
		Intents: 33280,
	}
	marshalledIdentifyData, err := json.Marshal(identifyData)
	if err != nil {
		return err
	}
	identifyDataPayload := GatewayEventPayload{
		Op:   2,
		Data: marshalledIdentifyData,
	}
	p, err := json.Marshal(identifyDataPayload)
	if err != nil {
		return err
	}
	g.WriteChan <- p

	return nil
}

func (g *GatewayConnection) startHeartbeat(heartbeatInterval int) {
	g.Wg.Add(1)
	hbTicker := time.NewTicker(time.Duration(heartbeatInterval) * time.Millisecond)
	go func() {
		defer g.Wg.Done()
		defer hbTicker.Stop()
		log.Println("Heartbeat started.")
		for {
			select {
			case <-g.Ctx.Done():
				log.Println("Heartbeat stopped.")
				return
			case <-hbTicker.C:
				err := g.sendHeartbeatEvent()
				if err != nil {
					g.ErrChan <- err
					return
				}
				hbTimeout := time.NewTimer(5 * time.Second)
				defer hbTimeout.Stop()
				select {
				case <-g.Ctx.Done():
					log.Println("Heartbeat stopped.")
					return
				case <-hbTimeout.C:
					log.Println("Heartbeat timeout.")
					g.ErrChan <- errors.New("heartbeat timeout")
					return
				case <-g.ACKChan:
					log.Println("Heartbeat ACK received.")
					hbTimeout.Stop()
				}

			}
		}
	}()
}

func (g *GatewayConnection) sendHeartbeatEvent() error {
	g.Mut.Lock()
	marshalledLSN, err := json.Marshal(g.LastSequenceNumber)
	g.Mut.Unlock()
	if err != nil {
		log.Println("Error marshalling last sequence number.", err)
		return err
	}
	heartBeatPayload := GatewayEventPayload{
		Op:   1,
		Data: marshalledLSN,
	}
	p, err := json.Marshal(heartBeatPayload)
	if err != nil {
		log.Println("Error marshalling heartbeat payload.", err)
		return err
	}
	log.Println("Sending Heartbeat | " + string(p))
	g.WriteChan <- p
	return nil
}

func (g *GatewayConnection) beginWriter() {
	g.Wg.Add(1)
	go func() {
		defer g.Wg.Done()
		for {
			select {
			case <-g.Ctx.Done():
				log.Println("Writer stopped.")
				return
			case msg := <-g.WriteChan:
				log.Println("Sending | " + string(msg))
				err := g.Conn.WriteMessage(websocket.TextMessage, msg)
				if err != nil {
					log.Println("Error writing message.", err)
					g.ErrChan <- err
					return
				}
			}
		}
	}()
}

func getGatewayUrl() (string, error) {
	client := &http.Client{}
	token := os.Getenv("TOKEN")
	req, err := http.NewRequest("GET", gatewayURLApiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Add("Authorization", "Bot "+token)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var gatewayResponse GatewayResponse
	err = json.Unmarshal(body, &gatewayResponse)
	if err != nil {
		return "", err
	}
	return gatewayResponse.URL + gatewayURLParams, nil
}

func (g *GatewayConnection) Start(gatewayURL string, messageChan chan Message) {
	go func() {
		dCh := make(chan bool)
		go g.connect(gatewayURL, false, dCh, messageChan)
		for t := range dCh {
			log.Println("Disconnected. Reconnecting...")
			if t {
				log.Println("Reconnecting with resume.")
				go g.connect(g.ResumeGatewayURL, t, dCh, messageChan)
			} else {
				log.Println("Reconnecting without resume.")
				go g.connect(gatewayURL, t, dCh, messageChan)
			}

		}
	}()

}
