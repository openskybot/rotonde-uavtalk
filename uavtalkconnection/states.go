package uavtalkconnection

import (
	"fmt"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/openskybot/skybot-router/common"
	"github.com/openskybot/skybot-router/dispatcher"
)

/*
 * This files needs refactoring for better state management.
 * This is where we abstract the whole uavtalk layer.
 * States are used to manage handshake and session management.
 * See https://github.com/TauLabs/TauLabs/wiki/UAVTalk-session-management
 * and https://wiki.openpilot.org/display/WIKI/UAVTalk
**/

type stateHolder struct {
	connection *dispatcher.Connection
	inChan     chan Packet
	outChan    chan Packet

	state             state
	autoAckAndPersist bool
	sessionID         uint16
}

// TODO: make thread safe.
func (sh *stateHolder) setState(s state) {
	sh.state = s
	sh.state.start()
}

type state interface {
	start()
	in(Packet) bool  // packets coming from the dispatcher
	out(Packet) bool // packets coming from the flight controller
}

type notConnected struct {
	stateHolder *stateHolder

	flightTelemetryStats *common.Definition
}

func (s *notConnected) start() {
	var err error
	s.flightTelemetryStats, err = definitions.GetDefinitionForName("FlightTelemetryStats")
	if err != nil {
		log.Fatal(err)
	}

	log.Info("Started notConnected state")

	handshakeReq := createGCSTelemetryStatsObjectPacket("HandshakeReq")
	s.stateHolder.inChan <- handshakeReq
}

func (s *notConnected) in(p Packet) bool {
	return false
}

func (s *notConnected) out(p Packet) bool {
	if p.definition == s.flightTelemetryStats {
		log.Info(p.data["Status"])
		if p.data["Status"] == "Disconnected" {
			handshakeReq := createGCSTelemetryStatsObjectPacket("HandshakeReq")
			s.stateHolder.inChan <- handshakeReq
		} else if p.data["Status"] == "HandshakeAck" {
			handshakeConnected := createGCSTelemetryStatsObjectPacket("Connected")
			s.stateHolder.inChan <- handshakeConnected
		} else if p.data["Status"] == "Connected" {
			s.stateHolder.setState(&noSession{stateHolder: s.stateHolder})
		}
	}
	return false
}

type noSession struct {
	stateHolder *stateHolder

	sessionID uint16

	currentObjectID uint8
	numberOfObjects uint8

	sessionManaging *common.Definition
}

func (s *noSession) start() {
	log.Info("Started noSession state")

	s.stateHolder.autoAckAndPersist = false

	var err error
	s.sessionManaging, err = definitions.GetDefinitionForName("SessionManaging")
	if err != nil {
		log.Fatal(err)
	}

	sessionManagingReq := createSessionManagingRequest()
	s.stateHolder.inChan <- sessionManagingReq
}

func (s *noSession) in(p Packet) bool {
	return false
}

/**
 * TODO: This is getting messy, and the initial object retrieval is not done, but the
 * process is still engaged on fc side, resulting in a 5 seconds pause of the stream,
 * which should be avoidable.
 */
func (s *noSession) out(p Packet) bool {
	if p.definition == s.sessionManaging {
		if p.cmd == objectCmd || p.cmd == objectCmdWithAck {
			numberOfObjects := p.data["NumberOfObjects"].(uint8)
			if numberOfObjects != 0 {
				s.numberOfObjects = numberOfObjects
			}
			if p.cmd == objectCmdWithAck {
				sessionManagingPacketAck := createPacketAck(p.definition)
				s.stateHolder.inChan <- sessionManagingPacketAck

				objectID := p.data["ObjectID"].(uint32)
				if objectID != 0 {
					definition, err := definitions.GetDefinitionForObjectID(objectID)
					if err != nil {
						log.Warning(err)
					} else {
						s.stateHolder.connection.OutChan <- *definition
						s.stateHolder.connection.OutChan <- *definition.Meta
					}
				}

				if s.currentObjectID >= s.numberOfObjects {
					s.stateHolder.sessionID = s.sessionID
					s.stateHolder.autoAckAndPersist = true
					s.stateHolder.setState(&stream{})
					return false
				}

				if s.currentObjectID == 0 {
					s.sessionID = uint16(time.Now().Unix())
					log.Info("Creating session ", s.sessionID)
				}
				sessionManagingPacket := createSessionManagingPacket(s.sessionID, s.currentObjectID)
				s.currentObjectID++
				s.stateHolder.inChan <- sessionManagingPacket
			} else {
				sessionID := p.data["SessionID"].(uint16)
				// partial and bad session recovery
				log.Info("got sessionID ", sessionID)
				if s.stateHolder.sessionID != 0 && s.stateHolder.sessionID == sessionID {
					log.Info("Recovering ", s.stateHolder.sessionID)
					s.stateHolder.setState(&stream{})
					return false
				}
				sessionManagingPacket := createSessionManagingPacket(0, 0)
				s.stateHolder.inChan <- sessionManagingPacket
			}
		} else if p.cmd == objectAck {
			log.Info("Received Ack for SessionManaging")
		} else if p.cmd == objectNack {
			log.Info("Received Nack for SessionManaging")
		}
	}
	return false
}

type stream struct {
}

func (s *stream) start() {

}

func (s *stream) in(p Packet) bool {
	return true
}

func (s *stream) out(p Packet) bool {
	return true
}

func newStateHolder(d *dispatcher.Dispatcher) *stateHolder {
	sh := &stateHolder{}
	sh.connection = dispatcher.NewConnection()
	sh.inChan = make(chan Packet, dispatcher.ChanQueueLength)
	sh.outChan = make(chan Packet, dispatcher.ChanQueueLength)

	d.AddConnection(sh.connection)

	sh.setState(&notConnected{stateHolder: sh})

	// cache for later use
	objectPersistenceDefinition, err := definitions.GetDefinitionForName("ObjectPersistence")
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		for {
			packet := <-sh.outChan

			if sh.autoAckAndPersist == true {
				if packet.cmd == objectCmdWithAck {
					sh.inChan <- createPacketAck(packet.definition)
				} else if packet.cmd == objectAck {
					// send ObjectPersistence when received a Ack for object with Settings == true
					if packet.definition != objectPersistenceDefinition && packet.definition.Settings == true {
						sh.inChan <- createPersistObject(packet.definition, packet.instanceID)
					}
				}
			}

			if sh.state.out(packet) == true {
				dispatcherPacket, err := newDispatherPacketFromPacket(packet)
				if err != nil {
					log.Warning(err)
					continue
				}
				sh.connection.OutChan <- dispatcherPacket
			}
		}
	}()

	// TODO implement auto-resend un-acked packet with cmd == objectCmdWithAck
	go func() {
		for {
			dispatcherPacket := <-sh.connection.InChan

			packet, err := newPacketFromDispatcher(dispatcherPacket)
			if err != nil {
				log.Warning(err)
				continue
			}
			if sh.state.in(*packet) {
				sh.inChan <- *packet
			}
		}
	}()

	return sh
}

func createGCSTelemetryStatsObjectPacket(status string) Packet {
	definition, err := definitions.GetDefinitionForName("GCSTelemetryStats")
	if err != nil {
		log.Fatal(err)
	}
	packet := newPacket(definition, objectCmd, 0, map[string]interface{}{
		"Status":     status,
		"TxDataRate": float64(0),
		"RxDataRate": float64(0),
		"TxFailures": float64(0),
		"RxFailures": float64(0),
		"TxRetries":  float64(0),
	})
	return *packet
}

func createSessionManagingRequest() Packet {
	definition, err := definitions.GetDefinitionForName("SessionManaging")
	if err != nil {
		log.Fatal(err)
	}
	packet := newPacket(definition, objectRequest, 0, map[string]interface{}{})
	return *packet
}

func createSessionManagingPacket(sessionID uint16, objectOfInterestIndex uint8) Packet {
	definition, err := definitions.GetDefinitionForName("SessionManaging")
	if err != nil {
		log.Fatal(err)
	}
	packet := newPacket(definition, objectCmd, 0, map[string]interface{}{
		"SessionID":             float64(sessionID),
		"ObjectID":              float64(0),
		"ObjectInstances":       float64(0),
		"NumberOfObjects":       float64(0),
		"ObjectOfInterestIndex": float64(objectOfInterestIndex),
	})
	return *packet
}

func createPersistObject(definition *common.Definition, instanceID uint16) Packet {
	objectPersistenceDefinition, err := definitions.GetDefinitionForName("ObjectPersistence")
	if err != nil {
		log.Fatal(err)
	}
	packet := newPacket(objectPersistenceDefinition, objectCmdWithAck, instanceID, map[string]interface{}{
		"ObjectID":   float64(definition.ObjectID),
		"InstanceID": float64(instanceID),
		"Selection":  "SingleObject",
		"Operation":  "Save",
	})
	return *packet
}

func createPacketAck(definition *common.Definition) Packet {
	packet := newPacket(definition, objectAck, 0, map[string]interface{}{})
	return *packet
}

func newPacketFromDispatcher(dispatcherPacket interface{}) (*Packet, error) {
	switch dp := dispatcherPacket.(type) {
	case dispatcher.Update:
		definition, err := definitions.GetDefinitionForObjectID(dp.ObjectID)
		if err != nil {
			return nil, err
		}
		var cmd uint8
		if definition.TelemetryFlight.Acked == true {
			cmd = objectCmdWithAck
		}
		return newPacket(definition, cmd, dp.InstanceID, dp.Data), nil
	case dispatcher.Request:
		definition, err := definitions.GetDefinitionForObjectID(dp.ObjectID)
		if err != nil {
			return nil, err
		}
		return newPacket(definition, objectRequest, dp.InstanceID, map[string]interface{}{}), nil
	default:
		return nil, fmt.Errorf("Only Update or Request packets con go through USB connection")
	}
}

func newDispatherPacketFromPacket(packet Packet) (interface{}, error) {
	if packet.cmd == objectCmd || packet.cmd == objectCmdWithAck {
		return dispatcher.Update{ObjectID: packet.definition.ObjectID, InstanceID: packet.instanceID, Data: packet.data}, nil
	}
	return nil, fmt.Errorf("Only packets with cmd == objectCmd or cmd == objectCmdWithAck can go out of the flight controller")
}
