package kafka

// scram.go wires the github.com/xdg-go/scram library into the sarama
// SCRAMClient interface so that SCRAM-SHA-256 and SCRAM-SHA-512 SASL
// mechanisms are available without pulling in any additional Kafka-specific
// helpers.
//
// This follows the same pattern used by the official sarama examples and
// Telegraf's Kafka plugin.

import (
	"crypto/sha256"
	"crypto/sha512"
	"hash"

	"github.com/xdg-go/scram"
)

// hashGeneratorFcn is a function that produces a new hash.Hash — either
// sha256.New or sha512.New depending on the negotiated SASL mechanism.
type hashGeneratorFcn func() hash.Hash

var (
	sha256HashGenerator hashGeneratorFcn = sha256.New
	sha512HashGenerator hashGeneratorFcn = sha512.New
)

// scramClient implements sarama.SCRAMClient using the xdg-go/scram library.
// A fresh instance must be created for each connection attempt; use a
// SCRAMClientGeneratorFunc closure in the sarama config.
type scramClient struct {
	HashGeneratorFcn hashGeneratorFcn
	client           *scram.Client
	conversation     *scram.ClientConversation
}

// Begin initialises the SCRAM exchange.  Called once per connection by sarama.
func (s *scramClient) Begin(userName, password, authzID string) error {
	c, err := scram.HashGeneratorFcn(s.HashGeneratorFcn).NewClient(userName, password, authzID)
	if err != nil {
		return err
	}
	s.client = c
	s.conversation = c.NewConversation()
	return nil
}

// Step advances the SCRAM handshake by one round-trip.  Called repeatedly by
// sarama until Done returns true.
func (s *scramClient) Step(challenge string) (string, error) {
	return s.conversation.Step(challenge)
}

// Done reports whether the SCRAM exchange has completed successfully.
func (s *scramClient) Done() bool {
	return s.conversation.Done()
}
