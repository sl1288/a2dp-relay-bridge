# A2DP relay bridge

Play [Music Assistant](https://music-assistant.io) on Bluetooth headphones
anywhere in the house. The bridge receives audio from Music Assistant, encodes
it for Bluetooth (LDAC, aptX HD, aptX, AAC or SBC) and streams it over the
network to ESP32 relay nodes, which send it to the headphones over A2DP. With
several nodes, the one with the best reception takes over.

The nodes run the ESPHome component from
[esphome-a2dp-relay](https://github.com/sl1288/esphome-a2dp-relay).

```
Music Assistant ──Sendspin──▶ bridge ──TCP (encoded A2DP packets)──▶ ESP32 node ──A2DP──▶ headphones
                                 ▲                                       │
                                 └──── BLE reception, buttons, battery ──┘
```

## Features

- **One Music Assistant player per headphone** (Sendspin protocol), with
  metadata, volume and the headphone buttons (play/pause, next, previous)
  going back to Music Assistant.
- **Codecs:** LDAC (adaptive or fixed quality), aptX HD, aptX, AAC and SBC,
  negotiated per headphone; a preferred codec can be set per headphone.
- **Handover between nodes:** every node measures each headphone's BLE
  advertisements; the stream moves to the node with clearly better reception.
  Headphones that advertise with rotating private addresses are recognized
  through their identity resolving key (IRK), which a node can learn by
  pairing with the headphone over BLE.
- **Battery level** of the headphones (hands-free profile indicators).
- **Voice assistant:** a headphone's voice assistant button starts a Home
  Assistant Assist pipeline through a Wyoming satellite per headphone; the
  answer plays on the headphone.
- **Home Assistant integration** over MQTT discovery: battery, state, node,
  codec, reception, buttons to connect and disconnect, codec selection.
- **Web interface** (German and English): status, pairing, BLE identities,
  codec settings. Optional login through Home Assistant (admins only).

## Requirements

- Music Assistant 2.x with the Sendspin provider (default endpoint
  `ws://<music-assistant>:8927/sendspin`).
- One or more ESP32 nodes with the
  [a2dp_relay ESPHome component](https://github.com/sl1288/esphome-a2dp-relay),
  preferably on Ethernet (for example WT32-ETH01): Bluetooth audio and Wi-Fi
  share one radio on the ESP32.
- Docker, or Go 1.27 with the encoder libraries (see *Building*).

## Running with Docker

The image is published to the GitHub Container Registry for amd64 and arm64:

```sh
mkdir data
curl -o data/bridge.yaml https://raw.githubusercontent.com/sl1288/a2dp-relay-bridge/main/bridge.example.yaml
# edit data/bridge.yaml: Music Assistant URL and the nodes
docker run -d --name a2dp-relay-bridge --restart unless-stopped --network host \
  -v "$PWD/data:/data" ghcr.io/sl1288/a2dp-relay-bridge:latest
```

or with [docker-compose.example.yml](docker-compose.example.yml). Host
networking is recommended: the bridge connects to the nodes, and Home
Assistant connects to the voice satellites on TCP ports 10700 and up (one per
headphone).

Then open `http://<host>:8099`, scan for headphones on a node and pair them.

## Configuration

See [bridge.example.yaml](bridge.example.yaml) for all settings with their
defaults. The bridge keeps its own state (paired headphones, Music Assistant
player IDs, learned keys) in `state.json` next to the configuration; login
sessions of the web interface are kept in `sessions.json`.

### Home Assistant

- **MQTT:** set `mqtt.broker` (and credentials). Every headphone and node
  appears as a device through MQTT discovery.
- **Voice assistant:** add the *Wyoming Protocol* integration in Home
  Assistant with the bridge host and the headphone's port, shown on its card
  in the web interface (10700, 10701, ...).
- **Login:** set `web.auth.home_assistant` to the Home Assistant URL as your
  browser reaches it. The bridge uses Home Assistant's login (IndieAuth, no
  registration needed) and keeps its own session; by default only
  administrators are admitted. To show the interface in the Home Assistant
  sidebar, add it as a *Webpage* dashboard.

## Building

The encoders are linked through cgo. On Debian or Ubuntu:

```sh
sudo apt-get install libfdk-aac-dev libldacbt-enc-dev libfreeaptx-dev   # fdk-aac: non-free / multiverse
go build ./cmd/a2dp-bridge
go test ./...
./a2dp-bridge -config bridge.yaml
```

Or build the image: `docker build -t a2dp-relay-bridge .`

Development tools:

- `cmd/relayctl` talks to a single node directly (status, inquiry, connect,
  BLE scan, test tone). Stop the bridge first; a node serves one client.
- `cmd/sendspin-probe` registers a test player with Music Assistant and logs
  what it sends, without any Bluetooth hardware.

## Node protocol

The bridge and the nodes speak a small binary protocol over TCP port 6055,
defined in
[`protocol.h`](https://github.com/sl1288/esphome-a2dp-relay/blob/main/components/a2dp_relay/protocol.h)
of the component and mirrored in [`internal/node`](internal/node). New message
types are ignored by older peers, so bridge and nodes can be updated
independently within a protocol version.

## Licenses

The bridge is released under the [MIT License](LICENSE). It contains the SBC
codec of BlueZ (`internal/codec/sbc/libsbc`, LGPL 2.1, see its `COPYING.LIB`)
and links against libldacbt (Apache 2.0), libfreeaptx (LGPL 2.1) and fdk-aac
(Fraunhofer FDK AAC license, non-free in Debian). The container image includes
these libraries from Debian.
