package pkg

import (
	cryptoRand "crypto/rand"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
)

func (ps PubSub) PublishMessages() error {
	rand.Seed(time.Now().UnixNano())

	messagesLeft := ps.MessagesCount
	workers := 64

	wg := sync.WaitGroup{}
	wg.Add(workers)

	addMsg := make(chan *message.Message)

	for num := 0; num < workers; num++ {
		go func() {
			defer wg.Done()

			for msg := range addMsg {
				if err := ps.Publisher.Publish(ps.Topic, msg); err != nil {
					panic(err)
				}
			}
		}()
	}

	msgPayload, err := ps.payload()
	if err != nil {
		return err
	}

	start := time.Now()

	var uuidFunc func() string
	if ps.UUIDFunc != nil {
		uuidFunc = ps.UUIDFunc
	} else {
		uuidFunc = watermill.NewULID
	}

	for ; messagesLeft > 0; messagesLeft-- {
		msg := message.NewMessage(uuidFunc(), msgPayload)
		addMsg <- msg
	}
	close(addMsg)

	wg.Wait()

	elapsed := time.Now().Sub(start)

	fmt.Printf("added %d messages in %s, %f msg/s\n", ps.MessagesCount, elapsed, float64(ps.MessagesCount)/elapsed.Seconds())

	return nil
}

func newBinaryULID() string {
	bytes, err := ulid.MustNew(ulid.Now(), cryptoRand.Reader).MarshalBinary()
	if err != nil {
		panic(err)
	}
	return string(bytes)
}

func (ps PubSub) payload() ([]byte, error) {
	msgPayload := make([]byte, ps.MessageSize)
	_, err := rand.Read(msgPayload)
	if err != nil {
		return nil, err
	}

	return msgPayload, nil
}
