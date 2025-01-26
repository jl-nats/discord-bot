package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
)

type Bot struct {
	Connection  *GatewayConnection
	MessageChan chan Message
}

type CreateMessage struct {
	Content string `json:"content"`
}

func NewBot() *Bot {
	return &Bot{
		Connection:  nil,
		MessageChan: make(chan Message, 100),
	}
}

func (b *Bot) Start() {
	gatewayURL, err := getGatewayUrl()
	if err != nil {
		log.Fatal("Error getting gateway url:", err.Error())
	}
	gc := newGatewayConnection()
	msgCh := make(chan Message, 100)
	b.MessageChan = msgCh
	gc.Start(gatewayURL, b.MessageChan)

	b.Connection = gc

	b.ListenForMessages()
}

func (b *Bot) ListenForMessages() {
	go func() {
		for {
			select {
			case msg := <-b.MessageChan:
				if msg.Content == "!ping" {
					b.SendMessage(msg.ChannelID, "Pong!")

				}
			}
		}
	}()
}

func (b *Bot) SendMessage(channelID string, content string) {

	msg := CreateMessage{
		Content: content,
	}
	marshalledMsg, err := json.Marshal(msg)
	if err != nil {
		log.Println("Error marshalling message:", err.Error())
		return
	}
	log.Println("Replying... ", string(marshalledMsg))
	c := &http.Client{}
	req, err := http.NewRequest("POST", "https://discord.com/api/v10/channels/"+channelID+"/messages", bytes.NewBuffer(marshalledMsg))
	if err != nil {
		log.Println("Error creating request:", err.Error())
		return
	}
	req.Header.Add("Authorization", "Bot "+os.Getenv("TOKEN"))
	req.Header.Add("Content-Type", "application/json")
	res, err := c.Do(req)
	if res.StatusCode != 200 {
		log.Println("Error sending message:", res.Status)
		b, _ := io.ReadAll(res.Body)
		log.Println(string(b))

		return
	}
	if err != nil {
		log.Println("Error sending message:", err.Error())
	}

}
