package web

import "github.com/eraser-privacy/eraser/internal/broker"

// setBrokersForTest replaces the broker database wholesale. Test-only - it
// lives in a _test.go file so it is not compiled into the shipped binary,
// and it takes brokerMu so it cannot interleave with a mutateBrokers write
// in progress. Real writes go through mutateBrokers.
func (s *Server) setBrokersForTest(brokers []broker.Broker) {
	s.brokerMu.Lock()
	defer s.brokerMu.Unlock()
	s.brokerDB.Store(&broker.BrokerDatabase{Brokers: brokers})
}
