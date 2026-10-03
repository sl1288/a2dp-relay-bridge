package node

import "fmt"

// EventName returns a readable name for an A2DP event code.
func EventName(ev uint8) string {
	switch ev {
	case EvDisconnected:
		return "disconnected"
	case EvConnecting:
		return "connecting"
	case EvConnected:
		return "connected"
	case EvDisconnecting:
		return "disconnecting"
	case EvAudioStarted:
		return "audio started"
	case EvAudioSuspended:
		return "audio suspended"
	case EvAudioConfig:
		return "audio config"
	case EvMediaCtrlAck:
		return "media control ack"
	}
	return fmt.Sprintf("event(%d)", ev)
}

// KeyName returns a readable name for an AVRCP passthrough key.
func KeyName(k uint8) string {
	switch k {
	case KeyVolumeUp:
		return "volume up"
	case KeyVolumeDown:
		return "volume down"
	case KeyPlay:
		return "play"
	case KeyStop:
		return "stop"
	case KeyPause:
		return "pause"
	case KeyForward:
		return "forward"
	case KeyBackward:
		return "backward"
	}
	return fmt.Sprintf("key(0x%02X)", k)
}
