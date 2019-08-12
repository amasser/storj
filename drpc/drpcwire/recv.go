// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcwire

import (
	"bufio"
	"io"

	"storj.io/storj/drpc"
)

type Receiver struct {
	scanner *bufio.Scanner
	pending map[PacketID]payloadState
}

type payloadState struct {
	kind PayloadKind
	data []byte
}

func NewReceiver(r io.Reader) *Receiver {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 32*1024), MaxPacketSize)
	scanner.Split(bufio.SplitFunc(func(data []byte, atEOF bool) (int, []byte, error) {
		rem, _, ok, err := ParsePacket(data)
		switch advance := len(data) - len(rem); {
		case err != nil, !ok:
			return 0, nil, err
		case advance < 0, len(data) < advance:
			return 0, nil, drpc.InternalError.New("bad parse")
		default:
			return advance, data[:advance], nil
		}
	}))
	return &Receiver{
		scanner: scanner,
		pending: make(map[PacketID]payloadState),
	}
}

// ReadPacket reads a fully formed Packet from the underlying reader and returns it. It
// handles message cancellation
func (r *Receiver) ReadPacket() (p *Packet, err error) {
	var (
		rem []byte
		pkt Packet
		ok  bool
	)

	for {
		if !r.scanner.Scan() {
			return nil, drpc.Error.Wrap(r.scanner.Err())
		}

		// the scanner should return us exactly one packet, so if there's remaining
		// bytes or if it didn't parse, then there's some internal error.
		rem, pkt, ok, err = ParsePacket(r.scanner.Bytes())
		if err != nil {
			return nil, drpc.InternalError.Wrap(err)
		} else if !ok {
			return nil, drpc.InternalError.New("invalid data returned from scanner")
		} else if len(rem) != 0 {
			return nil, drpc.InternalError.New("remaining bytes from parsing packet")
		} else if len(pkt.Data) != int(pkt.Length) {
			return nil, drpc.InternalError.New("invalid length of data and header length")
		}

		// get the payload state for the packet and ensure that the starting bit on the
		// frame is consistent with the payload state's existence.
		state, packetExists := r.pending[pkt.PacketID]
		if !packetExists && !pkt.Starting {
			return nil, drpc.ProtocolError.New("unknown packet id with no starting bit")
		} else if packetExists && pkt.Starting {
			return nil, drpc.ProtocolError.New("starting packet id that already exists")
		} else if packetExists && state.kind != pkt.PayloadKind {
			return nil, drpc.ProtocolError.New("changed payload kind for in flight message")
		}
		state = payloadState{
			kind: pkt.PayloadKind,
			data: append(state.data, pkt.Data...),
		}

		// if we have a complete packet. we no longer need any state about it and the
		// packet is now complete, so we set the data to the completed buffer.
		if !pkt.Continuation {
			delete(r.pending, pkt.PacketID)
			pkt.Data = state.data
			break
		}
		r.pending[pkt.PacketID] = state
	}

	// if we're returning an error packet, we can delete all the other pending messages
	// for the stream.
	if pkt.PayloadKind == PayloadKind_Error {
		for pid := range r.pending {
			if pid == pkt.PacketID {
				delete(r.pending, pid)
			}
		}
	}

	// we clear out out the frame info to only have the payload kind as it's the only
	// valid field for higher level consumers.
	pkt.FrameInfo = FrameInfo{PayloadKind: pkt.PayloadKind}
	return &pkt, nil
}
