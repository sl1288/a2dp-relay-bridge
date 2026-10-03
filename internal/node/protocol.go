// Package node implements the bridge side of the relay node protocol.
//
// The wire format mirrors components/a2dp_relay/protocol.h: every message is
// u8 type, u16 little-endian payload length, payload. Node messages have the
// high bit clear, bridge commands have it set.
package node

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ProtocolVersion is the version this bridge speaks. Version 2 carries raw
// codec information in A2DP events and adds codec preferences. Later
// additions are new message types; unknown ones are skipped (Unknown).
const ProtocolVersion = 2

const (
	headerLen  = 3
	maxPayload = 2048
)

// Node -> bridge message types.
const (
	MsgHello         = 0x01
	MsgStatus        = 0x02
	MsgAdv           = 0x03
	MsgInquiryResult = 0x04
	MsgInquiryDone   = 0x05
	MsgAuth          = 0x06
	MsgA2DPEvent     = 0x07
	MsgAVRCPKey      = 0x08
	MsgVolume        = 0x09
	MsgBonds         = 0x0A
	MsgCredit        = 0x0B
	MsgPong          = 0x0C
	MsgRemoteCaps    = 0x0D
	MsgBattery       = 0x0E
	MsgVoice         = 0x0F
	MsgVoiceAudio    = 0x10
	MsgBLEPair       = 0x11
)

// BLE pairing results in BLEPair.Status.
const (
	BLEPairOK            = 0
	BLEPairBusy          = 1 // another BLE pairing runs on the node
	BLEPairConnectFailed = 2 // no BLE connection
	BLEPairAuthFailed    = 3 // the headphone refused or aborted pairing
	BLEPairNoIRK         = 4 // paired, but no identity key was sent
	BLEPairTimeout       = 5
	BLEPairUnsupported   = 6 // node firmware without ble_pairing
)

// BLEPair is the result of a BLE pairing that learns a headphone's identity key.
type BLEPair struct {
	Addr         Addr  // the address the pairing was started with
	Status       uint8 // BLEPair* constants
	Reason       uint8 // SMP, HCI or GATT code on failure
	IdentityType uint8 // 0 public, 1 random static
	Identity     Addr
	IRK          IRK
}

// Voice events in Voice.Event.
const (
	VoiceRequested = 1 // the headphone asked for the voice assistant
	VoiceCancelled = 2 // the headphone ended it
	VoiceAudioOn   = 3 // voice connection open
	VoiceAudioOff  = 4 // voice connection closed
)

// VoiceCodec is the coding of voice packets.
type VoiceCodec uint8

const (
	VoiceCVSD VoiceCodec = 1 // 16-bit linear PCM, 8 kHz
	VoiceMSBC VoiceCodec = 2 // mSBC with H2 header, 16 kHz
)

// Voice reports the voice assistant state of a headphone.
type Voice struct {
	Addr      Addr
	Event     uint8
	Codec     VoiceCodec
	FrameSize int
}

// VoiceAudio is one microphone packet as received over (e)SCO.
type VoiceAudio struct {
	Bad  bool
	Data []byte
}

// Bridge -> node command types.
const (
	CmdHello        = 0x80
	CmdScan         = 0x81
	CmdInquiry      = 0x82
	CmdConnect      = 0x83
	CmdDisconnect   = 0x84
	CmdMediaStart   = 0x85
	CmdMediaSuspend = 0x86
	CmdMedia        = 0x87
	CmdSetVolume    = 0x88
	CmdGetBonds     = 0x89
	CmdRemoveBond   = 0x8A
	CmdFlush        = 0x8B
	CmdSetSBCCaps   = 0x8C
	CmdPing         = 0x8D
	CmdSetScanMode  = 0x8E
	CmdSetCodecs    = 0x8F
	CmdVoice        = 0x90
	CmdVoiceAudio   = 0x91
	CmdBLEPair      = 0x92
)

// MaxScanFilters is the number of scan filters a node keeps.
const MaxScanFilters = 16

// CodecID identifies a codec in CmdSetCodecs (mirrors CodecId in codecs.h).
type CodecID uint8

const (
	CodecIDSBC    CodecID = 0
	CodecIDAAC    CodecID = 1
	CodecIDAptX   CodecID = 2
	CodecIDAptXHD CodecID = 3
	CodecIDLDAC   CodecID = 4
)

// Codec bits in Hello.Codecs.
const (
	CodecSBC    = 1 << 0
	CodecAAC    = 1 << 1
	CodecAptX   = 1 << 2
	CodecAptXHD = 1 << 3
	CodecLDAC   = 1 << 4
)

// LinkState is the A2DP link state of a node.
type LinkState uint8

const (
	LinkIdle LinkState = iota
	LinkConnecting
	LinkConnected
	LinkStreaming
)

func (s LinkState) String() string {
	switch s {
	case LinkIdle:
		return "idle"
	case LinkConnecting:
		return "connecting"
	case LinkConnected:
		return "connected"
	case LinkStreaming:
		return "streaming"
	}
	return fmt.Sprintf("state(%d)", uint8(s))
}

// A2DP event codes in A2DPEvent.Event.
const (
	EvDisconnected   = 0
	EvConnecting     = 1
	EvConnected      = 2
	EvDisconnecting  = 3
	EvAudioStarted   = 4
	EvAudioSuspended = 5
	EvAudioConfig    = 6
	EvMediaCtrlAck   = 7
)

// AVRCP passthrough key codes.
const (
	KeyVolumeUp   = 0x41
	KeyVolumeDown = 0x42
	KeyPlay       = 0x44
	KeyStop       = 0x45
	KeyPause      = 0x46
	KeyForward    = 0x4B
	KeyBackward   = 0x4C
)

// Addr is a Bluetooth device address in display order.
type Addr [6]byte

func (a Addr) String() string {
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", a[0], a[1], a[2], a[3], a[4], a[5])
}

// IsZero reports whether the address is all zero.
func (a Addr) IsZero() bool { return a == Addr{} }

// ParseAddr parses "AA:BB:CC:DD:EE:FF" (case-insensitive, ':' or '-').
func ParseAddr(s string) (Addr, error) {
	var a Addr
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ':' || r == '-' })
	if len(parts) != 6 {
		return a, fmt.Errorf("invalid Bluetooth address %q", s)
	}
	for i, p := range parts {
		var b byte
		if _, err := fmt.Sscanf(p, "%02x", &b); err != nil || len(p) != 2 {
			return a, fmt.Errorf("invalid Bluetooth address %q", s)
		}
		a[i] = b
	}
	return a, nil
}

// Hello is the first message a node sends.
type Hello struct {
	Version       uint8
	BTAddr        Addr
	Codecs        uint16
	QueueCapacity uint16
	Name          string
}

// Status is sent by the node once per second.
type Status struct {
	Link          LinkState
	Peer          Addr
	MTU           uint16
	QueuedPackets uint16
	QueuedUs      uint32
	SentPackets   uint32
	Underruns     uint32
	Dropped       uint32
	FreeHeap      uint32
	MinFreeHeap   uint32
	UptimeS       uint32
}

// Adv is a BLE advertisement of a watched device.
type Adv struct {
	Addr     Addr
	AddrType uint8
	RSSI     int8
	AdvType  uint8
	Name     string
	Company  int // Bluetooth SIG company ID from the manufacturer data, -1: none
}

// InquiryResult is a Classic device found during discovery.
type InquiryResult struct {
	Addr  Addr
	Class uint32
	RSSI  int8
	Name  string
}

// InquiryDone ends a discovery.
type InquiryDone struct{}

// Auth reports the result of pairing.
type Auth struct {
	Addr   Addr
	Status uint8 // 0 = success (esp_bt_status_t)
	Name   string
}

// A2DPEvent reports A2DP link changes and the negotiated codec.
type A2DPEvent struct {
	Event     uint8
	Addr      Addr
	MTU       uint16
	Detail    uint8
	CodecType uint8  // 0 = SBC, 2 = AAC, 0xFF = vendor, 0xFE = none
	Info      []byte // raw codec information (LOSC, media type, codec type, ...)
}

// RemoteSEP is one codec endpoint offered by a headphone.
type RemoteSEP struct {
	SEID uint8
	Info []byte // raw codec capabilities
}

// RemoteCaps lists the codec endpoints of the connected headphone.
type RemoteCaps struct {
	Addr Addr
	SEPs []RemoteSEP
}

// AVRCPKey is a button press on the headphone.
type AVRCPKey struct {
	Addr     Addr
	Key      uint8
	Released bool
}

// Volume reports the headphone's absolute volume (0..127).
type Volume struct {
	Addr   Addr
	Volume uint8
	Origin uint8 // 0 = changed on the headphone, 1 = response to a set
}

// Bonds lists the headphones the node is paired with.
type Bonds []Addr

// Credit is the media flow-control report, sent every 20 ms while streaming.
type Credit struct {
	QueuedPackets uint16
	QueuedUs      uint32
	SentPackets   uint32
	Underruns     uint32
	// StackBusy counts how often the node's Bluetooth stack queue was full,
	// i.e. the radio link could not keep up with the bit rate.
	StackBusy uint32
}

// Pong answers a ping.
type Pong struct{ Token uint32 }

// BatterySource tells how a headphone reported its battery level.
type BatterySource uint8

const (
	BatteryApple       BatterySource = 1 // AT+IPHONEACCEV, steps of 10 %
	BatteryHFIndicator BatterySource = 2 // HFP 1.7 indicator (AT+BIEV)
	BatteryXEvent      BatterySource = 3 // Plantronics AT+XEVENT
)

func (s BatterySource) String() string {
	switch s {
	case BatteryApple:
		return "apple"
	case BatteryHFIndicator:
		return "hfp"
	case BatteryXEvent:
		return "xevent"
	}
	return fmt.Sprintf("source(%d)", uint8(s))
}

// Battery is a battery report of the connected headphone, received over the
// hands-free profile.
type Battery struct {
	Addr     Addr
	Percent  int
	Source   BatterySource
	Charging *bool         // nil if the headphone does not say
	Age      time.Duration // since the headphone reported it (replayed reports)
}

// Unknown is a message type this bridge does not know (sent by a newer node).
type Unknown struct{ Type byte }

var errShort = errors.New("node: message too short")

func readName(p []byte, off int) (string, error) {
	if len(p) < off+1 {
		return "", errShort
	}
	n := int(p[off])
	if len(p) < off+1+n {
		return "", errShort
	}
	return string(p[off+1 : off+1+n]), nil
}

// decode parses one node message payload.
func decode(typ byte, p []byte) (any, error) {
	le := binary.LittleEndian
	switch typ {
	case MsgHello:
		if len(p) < 13 {
			return nil, errShort
		}
		h := Hello{Version: p[0], Codecs: le.Uint16(p[7:]), QueueCapacity: le.Uint16(p[9:]), Name: string(p[13:])}
		copy(h.BTAddr[:], p[1:7])
		return h, nil
	case MsgStatus:
		if len(p) < 39 {
			return nil, errShort
		}
		s := Status{
			Link: LinkState(p[0]), MTU: le.Uint16(p[7:]), QueuedPackets: le.Uint16(p[9:]),
			QueuedUs: le.Uint32(p[11:]), SentPackets: le.Uint32(p[15:]), Underruns: le.Uint32(p[19:]),
			Dropped: le.Uint32(p[23:]), FreeHeap: le.Uint32(p[27:]), MinFreeHeap: le.Uint32(p[31:]),
			UptimeS: le.Uint32(p[35:]),
		}
		copy(s.Peer[:], p[1:7])
		return s, nil
	case MsgAdv:
		if len(p) < 10 {
			return nil, errShort
		}
		a := Adv{AddrType: p[6], RSSI: int8(p[7]), AdvType: p[8], Company: -1}
		copy(a.Addr[:], p[:6])
		var err error
		a.Name, err = readName(p, 9)
		// Older nodes end after the name.
		if off := 10 + len(a.Name); err == nil && len(p) >= off+2 {
			if c := le.Uint16(p[off:]); c != 0xFFFF {
				a.Company = int(c)
			}
		}
		return a, err
	case MsgInquiryResult:
		if len(p) < 12 {
			return nil, errShort
		}
		r := InquiryResult{Class: le.Uint32(p[6:]), RSSI: int8(p[10])}
		copy(r.Addr[:], p[:6])
		var err error
		r.Name, err = readName(p, 11)
		return r, err
	case MsgInquiryDone:
		return InquiryDone{}, nil
	case MsgAuth:
		if len(p) < 8 {
			return nil, errShort
		}
		a := Auth{Status: p[6]}
		copy(a.Addr[:], p[:6])
		var err error
		a.Name, err = readName(p, 7)
		return a, err
	case MsgA2DPEvent:
		if len(p) < 12 {
			return nil, errShort
		}
		e := A2DPEvent{Event: p[0], MTU: le.Uint16(p[7:]), Detail: p[9], CodecType: p[10]}
		copy(e.Addr[:], p[1:7])
		n := int(p[11])
		if len(p) < 12+n {
			return nil, errShort
		}
		e.Info = append([]byte(nil), p[12:12+n]...)
		return e, nil
	case MsgRemoteCaps:
		if len(p) < 7 {
			return nil, errShort
		}
		rc := RemoteCaps{}
		copy(rc.Addr[:], p[:6])
		off := 7
		for range int(p[6]) {
			if len(p) < off+2 || len(p) < off+2+int(p[off+1]) {
				return nil, errShort
			}
			n := int(p[off+1])
			rc.SEPs = append(rc.SEPs, RemoteSEP{SEID: p[off], Info: append([]byte(nil), p[off+2:off+2+n]...)})
			off += 2 + n
		}
		return rc, nil
	case MsgAVRCPKey:
		if len(p) < 8 {
			return nil, errShort
		}
		k := AVRCPKey{Key: p[6], Released: p[7] != 0}
		copy(k.Addr[:], p[:6])
		return k, nil
	case MsgVolume:
		if len(p) < 8 {
			return nil, errShort
		}
		v := Volume{Volume: p[6], Origin: p[7]}
		copy(v.Addr[:], p[:6])
		return v, nil
	case MsgBonds:
		if len(p) < 1 || len(p) < 1+int(p[0])*6 {
			return nil, errShort
		}
		b := make(Bonds, p[0])
		for i := range b {
			copy(b[i][:], p[1+i*6:])
		}
		return b, nil
	case MsgCredit:
		if len(p) < 18 {
			return nil, errShort
		}
		return Credit{QueuedPackets: le.Uint16(p), QueuedUs: le.Uint32(p[2:]), SentPackets: le.Uint32(p[6:]),
			Underruns: le.Uint32(p[10:]), StackBusy: le.Uint32(p[14:])}, nil
	case MsgPong:
		if len(p) < 4 {
			return nil, errShort
		}
		return Pong{Token: le.Uint32(p)}, nil
	case MsgBattery:
		if len(p) < 13 {
			return nil, errShort
		}
		b := Battery{Percent: min(int(p[6]), 100), Source: BatterySource(p[7]),
			Age: time.Duration(le.Uint32(p[9:])) * time.Second}
		copy(b.Addr[:], p[:6])
		if p[8]&batteryChargingKnown != 0 {
			charging := p[8]&batteryCharging != 0
			b.Charging = &charging
		}
		return b, nil
	case MsgVoice:
		if len(p) < 10 {
			return nil, errShort
		}
		v := Voice{Event: p[6], Codec: VoiceCodec(p[7]), FrameSize: int(le.Uint16(p[8:]))}
		copy(v.Addr[:], p[:6])
		return v, nil
	case MsgVoiceAudio:
		if len(p) < 1 {
			return nil, errShort
		}
		return VoiceAudio{Bad: p[0]&1 != 0, Data: append([]byte(nil), p[1:]...)}, nil
	case MsgBLEPair:
		if len(p) < 31 {
			return nil, errShort
		}
		r := BLEPair{Status: p[6], Reason: p[7], IdentityType: p[8]}
		copy(r.Addr[:], p[:6])
		copy(r.Identity[:], p[9:15])
		copy(r.IRK[:], p[15:31])
		return r, nil
	}
	return Unknown{Type: typ}, nil
}

// Battery report flags.
const (
	batteryChargingKnown = 1 << 0
	batteryCharging      = 1 << 1
)

// ScanFilter selects BLE advertisements a node reports.
type ScanFilter struct {
	Addr       *Addr  // exact address match
	NamePrefix string // or name prefix match
	IRK        *IRK   // or resolvable private addresses of this identity key
}

// ScanMode selects which advertisements a node reports.
type ScanMode uint8

const (
	ScanOff     ScanMode = 0
	ScanWatched ScanMode = 1
	ScanAll     ScanMode = 2
)

// SBCCaps are the SBC capabilities a node offers (bit masks as in the A2DP CIE).
type SBCCaps struct {
	SampleRates, ChannelModes, BlockLengths, Subbands, Allocation uint8
	MinBitpool, MaxBitpool                                        uint8
}

func encodeScan(mode ScanMode, active bool, interval, window uint16, filters []ScanFilter) []byte {
	p := []byte{byte(mode), 0, 0, 0, 0, 0, 0}
	if active {
		p[1] = 1
	}
	binary.LittleEndian.PutUint16(p[2:], interval)
	binary.LittleEndian.PutUint16(p[4:], window)
	n := 0
	for _, f := range filters {
		if n == MaxScanFilters {
			break
		}
		switch {
		case f.Addr != nil:
			p = append(p, 0, 6)
			p = append(p, f.Addr[:]...)
		case f.IRK != nil:
			p = append(p, 2, 16)
			p = append(p, f.IRK[:]...)
		case f.NamePrefix != "":
			name := f.NamePrefix
			if len(name) > 32 {
				name = name[:32]
			}
			p = append(p, 1, byte(len(name)))
			p = append(p, name...)
		default:
			continue
		}
		n++
	}
	p[6] = byte(n)
	return p
}

// CodecPrefs controls which codec a node negotiates when it connects a headphone.
type CodecPrefs struct {
	Order         []CodecID // most preferred first; SBC should always be included
	Prefer48k     bool
	SBCMaxBitpool uint8
	AACMaxBitrate uint32
}

func encodeCodecs(p CodecPrefs) []byte {
	b := []byte{0, p.SBCMaxBitpool, 0, 0, 0, 0, byte(len(p.Order))}
	if p.Prefer48k {
		b[0] = 1
	}
	binary.LittleEndian.PutUint32(b[2:], p.AACMaxBitrate)
	for _, id := range p.Order {
		b = append(b, byte(id))
	}
	return b
}

func encodeMedia(timestamp, durationUs uint32, frames uint16, data []byte) []byte {
	p := make([]byte, 10+len(data))
	binary.LittleEndian.PutUint32(p, timestamp)
	binary.LittleEndian.PutUint32(p[4:], durationUs)
	binary.LittleEndian.PutUint16(p[8:], frames)
	copy(p[10:], data)
	return p
}
