package sendspin

import "encoding/json"

// Wire types of the unencrypted Sendspin dialect accepted by Music Assistant
// 2.10 (aiosendspin 9.x in "legacy client" transition mode). Field names follow
// spec commit 8bfa0c5 plus the `available` field of aiosendspin 9.

const (
	coreVersion       = 1
	binaryAudioChunk  = 4
	audioHeaderLength = 9 // 1 byte message type + 8 bytes big-endian timestamp (µs)
)

type envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Format describes a PCM, FLAC or Opus stream format.
type Format struct {
	Codec      string `json:"codec"`
	Channels   int    `json:"channels"`
	SampleRate int    `json:"sample_rate"`
	BitDepth   int    `json:"bit_depth"`
}

// BytesPerFrame returns the size of one PCM frame (all channels).
func (f Format) BytesPerFrame() int { return f.Channels * f.BitDepth / 8 }

type deviceInfo struct {
	ProductName     string `json:"product_name,omitempty"`
	Manufacturer    string `json:"manufacturer,omitempty"`
	SoftwareVersion string `json:"software_version,omitempty"`
}

type playerSupport struct {
	SupportedFormats  []Format `json:"supported_formats"`
	BufferCapacity    int      `json:"buffer_capacity"`
	SupportedCommands []string `json:"supported_commands"`
}

type clientHello struct {
	ClientID       string         `json:"client_id"`
	Name           string         `json:"name"`
	DeviceInfo     *deviceInfo    `json:"device_info,omitempty"`
	Version        int            `json:"version"`
	SupportedRoles []string       `json:"supported_roles"`
	PlayerSupport  *playerSupport `json:"player@v1_support,omitempty"`
}

type serverHello struct {
	ServerID         string   `json:"server_id"`
	Name             string   `json:"name"`
	Version          int      `json:"version"`
	ActiveRoles      []string `json:"active_roles"`
	ConnectionReason string   `json:"connection_reason"`
}

type clientTime struct {
	ClientTransmitted int64 `json:"client_transmitted"`
}

type serverTime struct {
	ClientTransmitted int64 `json:"client_transmitted"`
	ServerReceived    int64 `json:"server_received"`
	ServerTransmitted int64 `json:"server_transmitted"`
}

// playerState is always sent complete: pointers are not needed because the
// client resends every field, which the spec explicitly allows.
type playerState struct {
	Volume             int      `json:"volume"`
	Muted              bool     `json:"muted"`
	StaticDelayMs      int      `json:"static_delay_ms"`
	RequiredLeadTimeMs int      `json:"required_lead_time_ms"`
	MinBufferMs        int      `json:"min_buffer_ms"`
	SupportedCommands  []string `json:"supported_commands"`
}

type clientState struct {
	Available bool         `json:"available"`
	Player    *playerState `json:"player,omitempty"`
}

type controllerCommand struct {
	Command    string `json:"command"`
	Volume     *int   `json:"volume,omitempty"`
	Mute       *bool  `json:"mute,omitempty"`
	PositionMs *int64 `json:"position_ms,omitempty"`
	OffsetMs   *int64 `json:"offset_ms,omitempty"`
}

type clientCommand struct {
	Controller *controllerCommand `json:"controller,omitempty"`
}

type clientGoodbye struct {
	Reason string `json:"reason"`
}

type streamStart struct {
	Player *struct {
		Format
		CodecHeader string `json:"codec_header,omitempty"`
	} `json:"player,omitempty"`
}

type streamRoles struct {
	Roles []string `json:"roles,omitempty"`
}

// affects reports whether a stream/clear or stream/end message targets role.
// An omitted role list addresses all roles.
func (s streamRoles) affects(role string) bool {
	if len(s.Roles) == 0 {
		return true
	}
	for _, r := range s.Roles {
		if r == role {
			return true
		}
	}
	return false
}

type serverCommand struct {
	Player *struct {
		Command       string `json:"command"`
		Volume        *int   `json:"volume,omitempty"`
		Mute          *bool  `json:"mute,omitempty"`
		StaticDelayMs *int   `json:"static_delay_ms,omitempty"`
	} `json:"player,omitempty"`
}

// Progress is the playback position reported with metadata.
type Progress struct {
	TrackProgressMs int64 `json:"track_progress"`
	TrackDurationMs int64 `json:"track_duration"`
	PlaybackSpeed   int   `json:"playback_speed"` // ×1000, 0 = paused
}

// Metadata describes the current track.
type Metadata struct {
	Timestamp   int64     `json:"timestamp"`
	Title       string    `json:"title,omitempty"`
	Artist      string    `json:"artist,omitempty"`
	AlbumArtist string    `json:"album_artist,omitempty"`
	Album       string    `json:"album,omitempty"`
	ArtworkURL  string    `json:"artwork_url,omitempty"`
	Year        int       `json:"year,omitempty"`
	Track       int       `json:"track,omitempty"`
	Progress    *Progress `json:"progress,omitempty"`
}

// ControllerState is the group state seen by the controller role.
type ControllerState struct {
	SupportedCommands []string `json:"supported_commands"`
	Volume            int      `json:"volume"`
	Muted             bool     `json:"muted"`
	Repeat            string   `json:"repeat"`
	Shuffle           bool     `json:"shuffle"`
}

type serverState struct {
	Metadata   json.RawMessage  `json:"metadata,omitempty"`
	Controller *ControllerState `json:"controller,omitempty"`
}

type groupUpdate struct {
	PlaybackState *string `json:"playback_state,omitempty"`
	GroupID       *string `json:"group_id,omitempty"`
	GroupName     *string `json:"group_name,omitempty"`
}
