package gonat

import (
	"io"
	"math/rand"
	"net"
	"syscall"
	"time"

	"github.com/getlantern/errors"
	"github.com/getlantern/ops"
	"github.com/ti-mo/conntrack"
)

type server struct {
	tcpSocket          io.ReadWriteCloser
	udpSocket          io.ReadWriteCloser
	downstream         io.ReadWriter
	opts               *Opts
	bufferPool         BufferPool
	ctrack             *conntrack.Conn
	ctTimeout          uint32
	randomPortSequence []uint16
	portIndexes        map[uint8]map[Addr]int
	connsByDownFT      map[FiveTuple]*conn
	connsByUpFT        map[FiveTuple]*conn
	fromDownstream     chan *IPPacket
	toDownstream       chan *IPPacket
	fromUpstream       chan *IPPacket
	closedConns        chan *conn
	close              chan interface{}
	closed             chan interface{}
}

// NewServer constructs a new Server that reads packets from downstream
// and writes response packets back to downstream.
func NewServer(downstream io.ReadWriter, opts *Opts) (Server, error) {
	err := opts.ApplyDefaults()
	if err != nil {
		return nil, errors.New("Error applying default options: %v", err)
	}

	log.Debugf("Outbound packets will use %v", opts.IFAddr)

	_ctTimeout := opts.IdleTimeout * 2
	if _ctTimeout < MinConntrackTimeout {
		_ctTimeout = MinConntrackTimeout
	}
	ctTimeout := uint32(_ctTimeout.Seconds())

	// We create a random order for assigning new ports to minimize the chance of colliding
	// with other running gonat instances.
	randomPortSequence := make([]uint16, numEphemeralPorts)
	for i := uint16(0); i < uint16(numEphemeralPorts); i++ {
		randomPortSequence[i] = minEphemeralPort + i
	}
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	rnd.Shuffle(numEphemeralPorts, func(i int, j int) {
		randomPortSequence[i], randomPortSequence[j] = randomPortSequence[j], randomPortSequence[i]
	})

	s := &server{
		downstream:         downstream,
		opts:               opts,
		bufferPool:         opts.BufferPool,
		ctTimeout:          ctTimeout,
		randomPortSequence: randomPortSequence,
		portIndexes:        make(map[uint8]map[Addr]int),
		connsByDownFT:      make(map[FiveTuple]*conn),
		connsByUpFT:        make(map[FiveTuple]*conn),
		fromDownstream:     make(chan *IPPacket, opts.BufferDepth),
		toDownstream:       make(chan *IPPacket, opts.BufferDepth),
		fromUpstream:       make(chan *IPPacket, opts.BufferDepth),
		closedConns:        make(chan *conn, opts.BufferDepth),
		close:              make(chan interface{}),
		closed:             make(chan interface{}),
	}
	return s, nil
}

func (s *server) Serve() error {
	var err error
	s.tcpSocket, err = createSocket(FiveTuple{IPProto: syscall.IPPROTO_TCP, Src: Addr{s.opts.IFAddr, 0}})
	if err != nil {
		return err
	}
	ops.Go(func() { s.readFromUpstream(s.tcpSocket) })

	s.udpSocket, err = createSocket(FiveTuple{IPProto: syscall.IPPROTO_UDP, Src: Addr{s.opts.IFAddr, 0}})
	if err != nil {
		s.tcpSocket.Close()
		return err
	}
	ops.Go(func() { s.readFromUpstream(s.udpSocket) })

	s.ctrack, err = conntrack.Dial(nil)
	if err != nil {
		s.tcpSocket.Close()
		s.udpSocket.Close()
		return errors.New("Unable to obtain connection for managing conntrack: %v", err)
	}

	s.opts.StatsTracker.start()
	ops.Go(s.dispatch)
	ops.Go(s.writeToDownstream)
	return s.readFromDownstream()
}

func (s *server) dispatch() {
	defer func() {
		for _, c := range s.connsByDownFT {
			c.Close()
			s.deleteConn(c)
			s.deleteConntrackEntry(c.upFT)
		}
		close(s.toDownstream)
		s.tcpSocket.Close()
		s.udpSocket.Close()
		s.ctrack.Close()
		close(s.closed)
	}()

	reapTicker := time.NewTicker(1 * time.Second)
	defer reapTicker.Stop()

	for {
		select {
		case pkt := <-s.fromDownstream:
			s.onPacketFromDownstream(pkt)
		case pkt := <-s.fromUpstream:
			s.onPacketFromUpstream(pkt)
		case c := <-s.closedConns:
			s.deleteConntrackEntry(c.upFT)
		case <-reapTicker.C:
			s.reapIdleConns()
		case <-s.close:
			return
		}
	}
}

func (s *server) onPacketFromDownstream(pkt *IPPacket) {
	switch pkt.IPProto {
	case syscall.IPPROTO_TCP, syscall.IPPROTO_UDP:
		s.opts.OnOutbound(pkt)
		downFT := pkt.FT()
		c := s.connsByDownFT[downFT]

		if pkt.HasTCPFlag(TCPFlagRST) {
			if c != nil {
				c.Close()
			}
			return
		}

		if c == nil {
			upFT, err := s.assignPort(downFT)
			if err != nil {
				log.Errorf("Unable to assign port, dropping packet %v: %v", downFT, err)
				s.dropPacket(pkt)
				return
			}
			c, err = s.newConn(downFT, upFT)
			if err != nil {
				log.Errorf("Unable to create connection, dropping packet %v: %v", downFT, err)
				s.dropPacket(pkt)
				return
			}
			s.connsByDownFT[downFT] = c
			s.connsByUpFT[upFT] = c
			c.markActive()
			s.opts.StatsTracker.addConn(pkt.IPProto)
		}
		select {
		case c.toUpstream <- pkt:
			log.Tracef("Transmit --  %v -> %v", c.downFT, c.upFT)
			s.opts.StatsTracker.acceptedPacket()
		default:
			// don't block if we're stalled writing upstream
			log.Tracef("Stalled writing packet %v upstream", downFT)
			s.dropPacket(pkt)
		}
	default:
		log.Tracef("Unknown IP protocol, ignoring packet %v: %v", pkt.FT(), pkt.IPProto)
		s.rejectPacket(pkt.Raw)
	}
}

func (s *server) onPacketFromUpstream(pkt *IPPacket) {
	upFT := pkt.FT().Reversed()
	c := s.connsByUpFT[upFT]
	if c == nil {
		log.Tracef("Ignoring packet for unknown upstream FT: %v", upFT)
		s.rejectPacket(pkt.Raw)
		return
	}

	pkt.SetDest(c.downFT.Src)
	s.opts.OnInbound(pkt, c.downFT)
	pkt.recalcChecksum()
	c.markActive()
	select {
	case s.toDownstream <- pkt:
		// okay
		log.Tracef("Transmit -- %v <- %v", c.downFT, c.upFT)
		s.opts.StatsTracker.acceptedPacket()
	default:
		log.Tracef("Stalled writing packet %v downstream", c.downFT)
		s.dropPacket(pkt)
	}
}

// assignPort assigns an ephemeral local port for a new connection. If an existing connection
// with the resulting 5-tuple is already tracked because a different application created it,
// this will fail on createConntrackEntry and then retry until it finds an untracked ephemeral
// port or runs out of ports to try.
func (s *server) assignPort(downFT FiveTuple) (upFT FiveTuple, err error) {
	portIndexesByOrigin := s.portIndexes[upFT.IPProto]
	if portIndexesByOrigin == nil {
		portIndexesByOrigin = make(map[Addr]int)
		s.portIndexes[upFT.IPProto] = portIndexesByOrigin
	}

	upFT.IPProto = downFT.IPProto
	upFT.Dst = downFT.Dst
	upFT.Src.IPString = s.opts.IFAddr

	for i := 0; i < numEphemeralPorts; i++ {
		portIndex := portIndexesByOrigin[downFT.Dst] + 1
		if portIndex >= numEphemeralPorts {
			// loop back around to beginning of random sequence
			portIndex = 0
		}
		portIndexesByOrigin[upFT.Dst] = portIndex
		upFT.Src.Port = s.randomPortSequence[portIndex]
		err = s.createConntrackEntry(upFT)
		if err != nil {
			// this can happen if this 5-tuple is already tracked, ignore and retry
			continue
		}
		return
	}
	err = errors.New("Gave up looking for ephemeral port, final error from conntrack: %v", err)
	return
}

func (s *server) reapIdleConns() {
	var connsToClose []*conn
	for _, c := range s.connsByDownFT {
		if c.timeSinceLastActive() > s.opts.IdleTimeout {
			connsToClose = append(connsToClose, c)
			s.deleteConn(c)
		}
	}
	if len(connsToClose) > 0 {
		// close conns on a goroutine to avoid tying up main dispatch loop
		ops.Go(func() {
			for _, c := range connsToClose {
				c.Close()
			}
		})
	}
}

func (s *server) deleteConn(c *conn) {
	delete(s.connsByDownFT, c.downFT)
	delete(s.connsByUpFT, c.upFT)
}

// readFromDownstream reads all IP packets from downstream clients.
func (s *server) readFromDownstream() error {
	defer s.Close()

	for {
		b := s.bufferPool.Get()
		n, err := s.downstream.Read(b)
		if err != nil {
			if err == io.EOF {
				return err
			}
			return errors.New("Unexpected error reading from downstream: %v", err)
		}
		raw := b[:n]
		pkt, err := parseIPPacket(raw)
		if err != nil {
			log.Tracef("Error on inbound packet, ignoring: %v", err)
			s.rejectPacket(raw)
			continue
		}
		s.fromDownstream <- pkt
	}
}

// writeToDownstream writes all IP packets that we're sending back dowstream.
func (s *server) writeToDownstream() {
	for pkt := range s.toDownstream {
		_, err := s.downstream.Write(pkt.Raw)
		s.bufferPool.Put(pkt.Raw)
		if err != nil {
			log.Errorf("Unexpected error writing to downstream: %v", err)
			return
		}
	}
}

func (s *server) readFromUpstream(socket io.ReadWriteCloser) {
	defer socket.Close()

	for {
		b := s.bufferPool.Get()
		n, err := socket.Read(b)
		if err != nil {
			s.rejectPacket(b)
			if netErr, ok := err.(net.Error); ok && netErr.Temporary() {
				continue
			}
			return
		}
		if pkt, err := parseIPPacket(b[:n]); err != nil {
			log.Tracef("Ignoring unparseable packet from upstream: %v", err)
			s.rejectPacket(b)
		} else {
			s.fromUpstream <- pkt
		}
	}
}

func (s *server) rejectPacket(b []byte) {
	s.opts.StatsTracker.invalidPacket()
	s.bufferPool.Put(b)
}

func (s *server) dropPacket(pkt *IPPacket) {
	s.opts.StatsTracker.droppedPacket()
	s.bufferPool.Put(pkt.Raw)
}

func (s *server) Close() error {
	select {
	case <-s.close:
		// already closed
	default:
		close(s.close)
	}
	<-s.closed
	return nil
}
