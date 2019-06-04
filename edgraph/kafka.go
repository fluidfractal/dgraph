package edgraph

import (
	"strings"
	"time"

	"github.com/Shopify/sarama"
	"github.com/dgraph-io/badger/pb"
	"github.com/golang/glog"
	"golang.org/x/net/context"
)

// Consumer represents a Sarama consumer group consumer
type Consumer struct {
	ready chan bool
}

// Setup is run at the beginning of a new session, before ConsumeClaim
func (consumer *Consumer) Setup(sarama.ConsumerGroupSession) error {
	// Mark the consumer as ready
	close(consumer.ready)
	return nil
}

// Cleanup is run at the end of a session, once all ConsumeClaim goroutines have exited
func (consumer *Consumer) Cleanup(sarama.ConsumerGroupSession) error {
	return nil
}

// ConsumeClaim must start a consumer loop of ConsumerGroupClaim's Messages().
func (consumer *Consumer) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for message := range claim.Messages() {
		var list pb.KVList
		if err := list.Unmarshal(message.Value); err != nil {
			glog.Errorf("error while unmarshaling from consumed message: %v", err)
			return err
		}

		loader := State.Pstore.NewLoader(16)
		for _, kv := range list.Kv {
			loader.Set(kv)
		}
		loader.Finish()

		glog.V(1).Infof("Message stored: value = %+v, timestamp = %v, topic = %s",
			list, message.Timestamp, message.Topic)

		session.MarkMessage(message, "")
	}

	return nil
}

// setupKafkaSource will create a kafka consumer and and use it to receive updates
func (s *ServerState) setupKafkaSource() {
	sourceBrokers := Config.KafkaOpt.SourceBrokers
	glog.Infof("source kafka brokers: %v", sourceBrokers)
	if len(sourceBrokers) > 0 {
		config := sarama.NewConfig()
		config.Version = sarama.V2_2_0_0
		/**
		 * Setup a new Sarama consumer group
		 */
		consumer := Consumer{
			ready: make(chan bool, 0),
		}

		ctx := context.Background()

		var client sarama.ConsumerGroup
		var err error
		for i := 0; i < 10; i++ {
			client, err = sarama.NewConsumerGroup(strings.Split(sourceBrokers, ","), kafkaGroup, config)
			if err == nil {
				break
			} else {
				glog.Errorf("unable to create the kafka consumer, "+
					"will retry in 5 seconds: %v", err)
				time.Sleep(5 * time.Second)
			}
		}

		if err != nil {
			panic(err)
		}

		go func() {
			for {
				err := client.Consume(ctx, []string{kafkaTopic}, &consumer)
				if err != nil {
					glog.Errorf("error while consuming from kafka: %v", err)
				}
			}
		}()

		<-consumer.ready // Await till the consumer has been set up
		glog.Info("kafka consumer up and running")
	}
}

// setupKafkaTarget will create a kafka producer and use it to send updates to
// the kafka cluster
func (s *ServerState) setupKafkaTarget() {
	targetBrokers := Config.KafkaOpt.TargetBrokers
	glog.Infof("target kafka brokers: %v", targetBrokers)
	if len(targetBrokers) > 0 {
		conf := sarama.NewConfig()
		conf.Producer.Return.Successes = true

		var producer sarama.SyncProducer
		var err error
		for i := 0; i < 10; i++ {
			producer, err = sarama.NewSyncProducer(strings.Split(targetBrokers, ","), conf)
			if err == nil {
				break
			} else {
				glog.Errorf("unable to create the kafka sync producer, "+
					"will retry in 5 seconds: %v", err)
				time.Sleep(5 * time.Second)
			}
		}
		if err != nil {
			glog.Errorf("unable to create the kafka sync producer, and will not publish updates")
			return
		}

		cb := func(list *pb.KVList) {
			// TODO: produce to kafka
			listBytes, err := list.Marshal()
			if err != nil {
				glog.Errorf("unable to marshal the kv list: %+v", err)
				return
			}
			_, _, err = producer.SendMessage(&sarama.ProducerMessage{
				Topic: kafkaTopic,
				Value: sarama.ByteEncoder(listBytes),
			})

			glog.V(1).Infof("produced a list with %d messages to kafka", len(list.Kv))
		}

		go func() {
			// The Subscribe will go into an infinite loop,
			// hence we need to run it inside a separate go routine
			if err := s.Pstore.Subscribe(context.Background(), cb, nil); err != nil {
				glog.Errorf("error while subscribing to the pstore: %v", err)
			}
		}()

		glog.V(1).Infof("subscribed to the pstore for updates")
	}
}