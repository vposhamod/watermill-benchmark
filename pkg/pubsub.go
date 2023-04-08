package pkg

import (
	stdSQL "database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Shopify/sarama"
	driver "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	natsOrg "github.com/nats-io/nats.go"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill-amqp/v2/pkg/amqp"
	"github.com/ThreeDotsLabs/watermill-googlecloud/pkg/googlecloud"
	"github.com/ThreeDotsLabs/watermill-kafka/v2/pkg/kafka"
	"github.com/ThreeDotsLabs/watermill-nats/v2/pkg/nats"
	"github.com/ThreeDotsLabs/watermill-sql/pkg/sql"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
)

const (
	defaultMessagesCount = 1000000
)

var logger = watermill.NopLogger{}

type PubSub struct {
	Publisher  message.Publisher
	Subscriber message.Subscriber

	MessagesCount int
	MessageSize   int

	Topic string

	UUIDFunc func() string
}

func NewPubSub(name string, topic string, messagesCount int, messageSize int) (PubSub, error) {
	definition, ok := pubSubDefinitions[name]
	if !ok {
		return PubSub{}, fmt.Errorf("unknown PubSub: %s", name)
	}

	pub, sub := definition.Constructor()

	if messagesCount == 0 {
		if definition.MessagesCount != 0 {
			messagesCount = definition.MessagesCount
		} else {
			messagesCount = defaultMessagesCount
		}
	}

	return PubSub{
		Publisher:  pub,
		Subscriber: sub,

		MessagesCount: messagesCount,
		MessageSize:   messageSize,
		Topic:         topic,

		UUIDFunc: definition.UUIDFunc,
	}, nil
}

func (ps PubSub) Close() error {
	if err := ps.Publisher.Close(); err != nil {
		return err
	}
	return ps.Subscriber.Close()
}

type PubSubDefinition struct {
	MessagesCount int
	UUIDFunc      func() string
	Constructor   func() (message.Publisher, message.Subscriber)
}

var pubSubDefinitions = map[string]PubSubDefinition{
	"gochannel": {
		Constructor: func() (message.Publisher, message.Subscriber) {
			pubsub := gochannel.NewGoChannel(gochannel.Config{
				Persistent: true,
			}, logger)
			return pubsub, pubsub
		},
	},
	"kafka": {
		MessagesCount: 20000000,
		Constructor:   kafkaConstructor([]string{"kafka:9092"}),
	},
	"kafka-multinode": {
		MessagesCount: 20000000,
		Constructor:   kafkaConstructor([]string{"kafka1:9091", "kafka2:9092", "kafka3:9093", "kafka4:9094", "kafka5:9095"}),
	},
	"nats": {
		Constructor: func() (message.Publisher, message.Subscriber) {
			natsURL := os.Getenv("WATERMILL_NATS_URL")
			if natsURL == "" {
				natsURL = "nats://nats:4222"
			}

			pub, err := nats.NewPublisher(nats.PublisherConfig{
				URL:         natsURL,
				NatsOptions: []natsOrg.Option{},
				Marshaler:   nats.GobMarshaler{},
			}, logger)
			if err != nil {
				panic(err)
			}

			sub, err := nats.NewSubscriber(nats.SubscriberConfig{
				URL:              natsURL,
				JetStream:        nats.JetStreamConfig{Disabled: false, TrackMsgId: true},
				SubscribersCount: 16, // todo - experiment
				Unmarshaler:      nats.GobMarshaler{},
				AckWaitTimeout:   time.Second,
			}, logger)
			if err != nil {
				panic(err)
			}

			return pub, sub
		},
	},
	"googlecloud": {
		Constructor: func() (message.Publisher, message.Subscriber) {
			pub, err := googlecloud.NewPublisher(
				googlecloud.PublisherConfig{
					ProjectID: os.Getenv("GOOGLE_CLOUD_PROJECT"),
					Marshaler: googlecloud.DefaultMarshalerUnmarshaler{},
				}, logger,
			)
			if err != nil {
				panic(err)
			}

			sub := NewMultiplier(
				func() (message.Subscriber, error) {
					subscriber, err := googlecloud.NewSubscriber(
						googlecloud.SubscriberConfig{
							ProjectID: os.Getenv("GOOGLE_CLOUD_PROJECT"),
							GenerateSubscriptionName: func(topic string) string {
								return topic
							},
							Unmarshaler: googlecloud.DefaultMarshalerUnmarshaler{},
						},
						logger,
					)
					if err != nil {
						return nil, err
					}

					return subscriber, nil
				}, 16,
			)

			return pub, sub
		},
	},
	"mysql": {
		MessagesCount: 30000,
		UUIDFunc:      newBinaryULID,
		Constructor: func() (message.Publisher, message.Subscriber) {
			conf := driver.NewConfig()
			conf.Net = "tcp"
			conf.User = "root"
			conf.Addr = "mysql"
			conf.DBName = "watermill"

			db, err := stdSQL.Open("mysql", conf.FormatDSN())
			if err != nil {
				panic(err)
			}

			err = db.Ping()
			if err != nil {
				panic(err)
			}

			pub, err := sql.NewPublisher(
				db,
				sql.PublisherConfig{
					AutoInitializeSchema: true,
					SchemaAdapter:        MySQLSchema{},
				},
				logger,
			)
			if err != nil {
				panic(err)
			}

			sub, err := sql.NewSubscriber(
				db,
				sql.SubscriberConfig{
					SchemaAdapter:    MySQLSchema{},
					OffsetsAdapter:   sql.DefaultMySQLOffsetsAdapter{},
					ConsumerGroup:    watermill.NewULID(),
					InitializeSchema: true,
				},
				logger,
			)
			if err != nil {
				panic(err)
			}

			return pub, sub
		},
	},
	"postgresql": {
		MessagesCount: 30000,
		UUIDFunc:      watermill.NewUUID,
		Constructor: func() (message.Publisher, message.Subscriber) {
			dsn := "postgres://watermill:password@postgres:5432/watermill?sslmode=disable"
			db, err := stdSQL.Open("postgres", dsn)
			if err != nil {
				panic(err)
			}

			err = db.Ping()
			if err != nil {
				panic(err)
			}

			pub, err := sql.NewPublisher(
				db,
				sql.PublisherConfig{
					AutoInitializeSchema: true,
					SchemaAdapter:        PostgreSQLSchema{},
				},
				logger,
			)
			if err != nil {
				panic(err)
			}

			sub, err := sql.NewSubscriber(
				db,
				sql.SubscriberConfig{
					SchemaAdapter:    PostgreSQLSchema{},
					OffsetsAdapter:   sql.DefaultPostgreSQLOffsetsAdapter{},
					ConsumerGroup:    watermill.NewULID(),
					InitializeSchema: true,
				},
				logger,
			)
			if err != nil {
				panic(err)
			}

			return pub, sub
		},
	},
	"amqp": {
		MessagesCount: 100000,
		Constructor: func() (message.Publisher, message.Subscriber) {
			config := amqp.NewDurablePubSubConfig(
				"amqp://rabbitmq:5672",
				func(topic string) string {
					return topic
				},
			)

			pub, err := amqp.NewPublisher(config, logger)
			if err != nil {
				panic(err)
			}

			sub := NewMultiplier(
				func() (message.Subscriber, error) {
					sub, err := amqp.NewSubscriber(config, logger)
					if err != nil {
						panic(err)
					}
					return sub, nil
				}, 16,
			)

			return pub, sub
		},
	},
}

func kafkaConstructor(brokers []string) func() (message.Publisher, message.Subscriber) {
	return func() (message.Publisher, message.Subscriber) {
		publisher, err := kafka.NewPublisher(
			kafka.PublisherConfig{
				Brokers:   brokers,
				Marshaler: kafka.DefaultMarshaler{},
			},
			logger,
		)
		if err != nil {
			panic(err)
		}

		saramaConfig := kafka.DefaultSaramaSubscriberConfig()
		saramaConfig.Consumer.Offsets.Initial = sarama.OffsetOldest

		subscriber, err := kafka.NewSubscriber(
			kafka.SubscriberConfig{
				Brokers:               brokers,
				Unmarshaler:           kafka.DefaultMarshaler{},
				OverwriteSaramaConfig: saramaConfig,
				ConsumerGroup:         "benchmark",
			},
			logger,
		)
		if err != nil {
			panic(err)
		}

		return publisher, subscriber
	}
}

type MySQLSchema struct {
	sql.DefaultMySQLSchema
}

func (m MySQLSchema) SchemaInitializingQueries(topic string) []string {
	createMessagesTable := strings.Join([]string{
		"CREATE TABLE IF NOT EXISTS " + m.MessagesTable(topic) + " (",
		"`offset` BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,",
		"`uuid` BINARY(16) NOT NULL,",
		"`created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,",
		"`payload` BLOB DEFAULT NULL,",
		"`metadata` JSON DEFAULT NULL",
		");",
	}, "\n")

	return []string{createMessagesTable}
}

type PostgreSQLSchema struct {
	sql.DefaultPostgreSQLSchema
}

func (p PostgreSQLSchema) SchemaInitializingQueries(topic string) []string {
	createMessagesTable := strings.Join([]string{
		`CREATE TABLE IF NOT EXISTS ` + p.MessagesTable(topic) + ` (`,
		`"offset" SERIAL PRIMARY KEY,`,
		`"uuid" UUID NOT NULL,`,
		`"created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,`,
		`"payload" BYTEA DEFAULT NULL,`,
		`"metadata" JSON DEFAULT NULL`,
		`);`,
	}, "\n")

	return []string{createMessagesTable}
}
