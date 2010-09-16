package server

import (
	"os"
	"net"
	
	"junta/util"
	"junta/paxos"
	"junta/proto"
	"junta/store"
	"strconv"
	"time"
)

type ReadFromWriteToer interface {
	ReadFrom([]byte) (int, net.Addr, os.Error)
	WriteTo([]byte, net.Addr) (int, os.Error)
	LocalAddr() net.Addr
}

const packetSize = 3000

type conn struct {
	net.Conn
	s *Server
}

type Manager interface {
	PutFrom(string, paxos.Msg)
	Propose(string) (uint64, string, os.Error)
	Alpha() int
}

type Server struct {
	Addr string
	St *store.Store
	Mg Manager
}

func (sv *Server) ListenAndServe() os.Error {
	logger := util.NewLogger("server %s", sv.Addr)

	logger.Log("binding")
	l, err := net.Listen("tcp", sv.Addr)
	if err != nil {
		logger.Log(err)
		return err
	}
	defer l.Close()
	logger.Log("listening")

	err = sv.Serve(l)
	if err != nil {
		logger.Logf("%s: %s", l, err)
}
	return err
}

func (sv *Server) ListenAndServeUdp(outs chan paxos.Packet) os.Error {
	logger := util.NewLogger("udp server %s", sv.Addr)

	logger.Log("binding")
	u, err := net.ListenPacket("udp", sv.Addr)
	if err != nil {
		logger.Log(err)
		return err
	}
	defer u.Close()
	logger.Log("listening")

	err = sv.ServeUdp(u, outs)
	if err != nil {
		logger.Logf("%s: %s", u, err)
	}
	return err
}

func (sv *Server) ServeUdp(u ReadFromWriteToer, outs chan paxos.Packet) os.Error {
	recvd := make(chan paxos.Packet)
	sent := make(chan paxos.Packet)

	logger := util.NewLogger("udp server %s", u.LocalAddr())
	go func() {
		logger.Log("reading messages...")
		for {
			msg, addr, err := paxos.ReadMsg(u, packetSize)
			if err != nil {
				logger.Log(err)
				continue
			}
			logger.Logf("read %v from %s", msg, addr)
			recvd <- paxos.Packet{msg, addr}
			sv.Mg.PutFrom(addr, msg)
		}
	}()

	go func() {
		logger.Log("sending messages...")
		for pk := range outs {
			logger.Logf("sending %v", pk)
			udpAddr, err := net.ResolveUDPAddr(pk.Addr)
			if err != nil {
				logger.Log(err)
				continue
			}

			_, err = u.WriteTo(pk.Msg.WireBytes(), udpAddr)
			if err != nil {
				logger.Log(err)
				continue
			}
			sent <- paxos.Packet{pk.Msg, pk.Addr}
		}
	}()

	needsAck := make(map[string]bool)
	resend := make(chan paxos.Packet)
	for {
		select {
		case pk := <-recvd:
			if pk.Msg.HasFlags(paxos.Ack) {
				logger.Logf("got ack %s %v", pk.Addr, pk.Msg)
				needsAck[pk.Id()] = false
			} else {
				logger.Logf("sending ack %s %v", pk.Addr, pk.Msg)
				udpAddr, err := net.ResolveUDPAddr(pk.Addr)
				if err != nil {
					break
				}
				ack := pk.Msg.Dup().SetFlags(paxos.Ack)
				u.WriteTo(ack.WireBytes(), udpAddr)
			}
		case pk := <-sent:
			needsAck[pk.Id()] = true
			logger.Logf("needs ack %s %v", pk.Addr, pk.Msg)
			go func() {
				time.Sleep(100000000) // ns == 0.1s
				resend <- pk
			}()
		case pk := <-resend:
			if needsAck[pk.Id()] {
				logger.Logf("resending %s %v", pk.Addr, pk.Msg)
				go func() {
					outs <- pk
				}()
			} else {
				needsAck[pk.Id()] = false, false
			}
		}
	}

	panic("not reached")
}

func (s *Server) Serve(l net.Listener) os.Error {
	for {
		rw, e := l.Accept()
		if e != nil {
			return e
		}
		c := &conn{rw, s}
		go c.serve()
	}

	panic("not reached")
}

func (sv *Server) setOnce(path, body, cas string) (uint64, os.Error) {
	mut, err := store.EncodeSet(path, body, cas)
	if err != nil {
		return 0, err
	}

	seqn, v, err := sv.Mg.Propose(mut)
	if err != nil {
		return 0, err
	}

	// We failed, but only because of a competing proposal. The client should
	// retry.
	if v != mut {
		return 0, os.EAGAIN
	}

	return seqn, nil
}

func (sv *Server) Set(path, body, cas string) (seqn uint64, err os.Error) {
	err = os.EAGAIN
	for err == os.EAGAIN {
		seqn, err = sv.setOnce(path, body, cas)
	}
	return
}

func (sv *Server) delOnce(path, cas string) (uint64, os.Error) {
	mut, err := store.EncodeDel(path, cas)
	if err != nil {
		return 0, err
	}

	seqn, v, err := sv.Mg.Propose(mut)
	if err != nil {
		return 0, err
	}

	// We failed, but only because of a competing proposal. The client should
	// retry.
	if v != mut {
		return 0, os.EAGAIN
	}

	return seqn, nil
}

func (sv *Server) Del(path, cas string) (seqn uint64, err os.Error) {
	err = os.EAGAIN
	for err == os.EAGAIN {
		seqn, err = sv.delOnce(path, cas)
	}
	return
}

func (sv *Server) WaitForPathSet(path string) (body string, err os.Error) {
	evs := make(chan store.Event)
	defer close(evs)
	sv.St.Watch(path, evs)

	parts, cas := sv.St.Lookup(path)
	if cas != store.Dir && cas != store.Missing {
		return parts[0], nil
	}

	for ev := range evs {
		if ev.IsSet() {
			return ev.Body, nil
		}
	}

	panic("not reached")
}

// Repeatedly propose nop values until a successful read from `done`.
func (sv *Server) AdvanceUntil(done chan int) {
	for _, ok := <-done; !ok; _, ok = <-done {
		sv.Mg.Propose(store.Nop)
	}
}

func (c *conn) serve() {
	pc := proto.NewConn(c)
	logger := util.NewLogger("%v", c.RemoteAddr())
	logger.Log("accepted connection")
	for {
		rid, parts, err := pc.ReadRequest()
		if err != nil {
			if err == os.EOF {
				logger.Log("connection closed by peer")
			} else {
				logger.Log(err)
			}
			return
		}

		rlogger := util.NewLogger("%v - req [%d]", c.RemoteAddr(), rid)
		rlogger.Logf("received <%v>", parts)

		if len(parts) == 0 {
			rlogger.Log("zero parts supplied")
			pc.SendError(rid, proto.InvalidCommand)
			continue
		}

		switch parts[0] {
		default:
			rlogger.Logf("unknown command <%s>", parts[0])
			pc.SendError(rid, proto.InvalidCommand)
		case "set":
			if len(parts) != 4 {
				rlogger.Logf("invalid set command: %#v", parts)
				pc.SendError(rid, "wrong number of parts")
				break
			}
			rlogger.Logf("set %q=%q (cas %q)", parts[1], parts[2], parts[3])
			_, err := c.s.Set(parts[1], parts[2], parts[3])
			if err != nil {
				rlogger.Logf("bad: %s", err)
				pc.SendError(rid, err.String())
			} else {
				rlogger.Logf("good")
				pc.SendResponse(rid, "true")
			}
		case "del":
			if len(parts) != 3 {
				rlogger.Logf("invalid del command: %v", parts)
				pc.SendError(rid, "wrong number of parts")
				break
			}
			rlogger.Logf("del %q (cas %q)", parts[1], parts[2])
			_, err := c.s.Del(parts[1], parts[2])
			if err != nil {
				rlogger.Logf("bad: %s", err)
				pc.SendError(rid, err.String())
			} else {
				rlogger.Logf("good")
				pc.SendResponse(rid, "true")
			}
		case "wait-for-path-set": // TODO this is for demo purposes only
			if len(parts) != 2 {
				rlogger.Logf("invalid del command: %v", parts)
				pc.SendError(rid, "wrong number of parts")
				break
			}
			rlogger.Logf("wait-for-path-set %q", parts[1])
			body, err := c.s.WaitForPathSet(parts[1])
			if err != nil {
				rlogger.Logf("bad: %s", err)
				pc.SendError(rid, err.String())
			} else {
				rlogger.Logf("good %q", body)
				pc.SendResponse(rid, body)
			}
		case "join":
			// join abc123 1.2.3.4:999
			if len(parts) != 3 {
				rlogger.Logf("invalid join command: %v", parts)
				pc.SendError(rid, "wrong number of parts")
				break
			}

			who, addr := parts[1], parts[2]
			rlogger.Logf("membership requested for %s at %s", who, addr)

			key := "/j/junta/members/" + who

			seqn, err := c.s.Set(key, addr, store.Missing)
			if err != nil {
				rlogger.Logf("bad: %s", err)
				pc.SendError(rid, err.String())
			} else {
				rlogger.Logf("good")
				done := make(chan int)
				go c.s.AdvanceUntil(done)
				c.s.St.Sync(seqn + uint64(c.s.Mg.Alpha()))
				close(done)
				seqn, snap := c.s.St.Snapshot()
				pc.SendResponse(rid, strconv.Uitoa64(seqn), snap)
			}
		}
	}
}
