// Command relayctl talks to a single relay node for testing: it prints every
// event the node sends and can run one command.
//
//	relayctl [-node host:port] [-for 20s] <command> [args]
//
// Commands: status, bonds, inquiry <seconds>, connect <addr>, disconnect,
// unpair <addr>, start, suspend, volume <0..127>, discoverable on|off,
// scan all | scan off | scan name <prefix>... | scan addr <addr>...
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/sl1288/a2dp-relay-bridge/internal/node"
)

func main() {
	nodeAddr := flag.String("node", "a2dp-relay.local:6055", "node address (host:port)")
	runFor := flag.Duration("for", 15*time.Second, "how long to print events")
	verbose := flag.Bool("v", false, "also print status and credit messages")
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 {
		args = []string{"status"}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *runFor)
	defer cancel()

	c, err := node.Dial(ctx, *nodeAddr)
	if err != nil {
		fail(err)
	}
	defer c.Close()

	events := make(chan any, 256)
	done := make(chan struct{})
	go func() {
		defer close(done)
		printEvents(c, *verbose || args[0] == "status", events)
	}()

	if args[0] == "tone" {
		secs, freq := 10, 440.0
		if len(args) > 1 {
			if v, err := strconv.Atoi(args[1]); err == nil {
				secs = v
			}
		}
		if len(args) > 2 {
			if v, err := strconv.ParseFloat(args[2], 64); err == nil {
				freq = v
			}
		}
		if err := tone(ctx, c, events, time.Duration(secs)*time.Second, freq); err != nil {
			fail(err)
		}
		return
	}
	if err := run(c, args); err != nil {
		fail(err)
	}
	select {
	case <-ctx.Done():
	case <-done:
	}
}

func run(c *node.Conn, args []string) error {
	need := func(n int) error {
		if len(args) < n+1 {
			return fmt.Errorf("%s needs %d argument(s)", args[0], n)
		}
		return nil
	}
	switch args[0] {
	case "status":
		return nil
	case "bonds":
		return c.GetBonds()
	case "inquiry":
		secs := 10
		if len(args) > 1 {
			v, err := strconv.Atoi(args[1])
			if err != nil {
				return err
			}
			secs = v
		}
		return c.Inquiry(secs)
	case "connect", "unpair":
		if err := need(1); err != nil {
			return err
		}
		a, err := node.ParseAddr(args[1])
		if err != nil {
			return err
		}
		if args[0] == "connect" {
			return c.Connect(a)
		}
		return c.RemoveBond(a)
	case "disconnect":
		return c.Disconnect(node.Addr{})
	case "start":
		return c.MediaStart()
	case "suspend":
		return c.MediaSuspend()
	case "volume":
		if err := need(1); err != nil {
			return err
		}
		v, err := strconv.Atoi(args[1])
		if err != nil {
			return err
		}
		return c.SetVolume(uint8(min(max(v, 0), 127)))
	case "codecs":
		// codecs LDAC "aptX HD" AAC SBC  (order of preference)
		if err := need(1); err != nil {
			return err
		}
		p := node.CodecPrefs{Prefer48k: true, SBCMaxBitpool: 53, AACMaxBitrate: 320000}
		for _, name := range args[1:] {
			id, ok := node.CodecIDByName[name]
			if !ok {
				return fmt.Errorf("unknown codec %q", name)
			}
			p.Order = append(p.Order, id)
		}
		return c.SetCodecs(p)
	case "discoverable":
		if err := need(1); err != nil {
			return err
		}
		return c.SetScanMode(true, args[1] == "on")
	case "scan":
		if err := need(1); err != nil {
			return err
		}
		switch args[1] {
		case "off":
			return c.Scan(node.ScanOff, false, 100*time.Millisecond, 30*time.Millisecond, nil)
		case "all":
			return c.Scan(node.ScanAll, true, 100*time.Millisecond, 50*time.Millisecond, nil)
		case "name", "addr":
			var filters []node.ScanFilter
			for _, v := range args[2:] {
				if args[1] == "name" {
					filters = append(filters, node.ScanFilter{NamePrefix: v})
					continue
				}
				a, err := node.ParseAddr(v)
				if err != nil {
					return err
				}
				filters = append(filters, node.ScanFilter{Addr: &a})
			}
			return c.Scan(node.ScanWatched, true, 100*time.Millisecond, 30*time.Millisecond, filters)
		}
		return fmt.Errorf("unknown scan mode %q", args[1])
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func printEvents(c *node.Conn, verbose bool, events chan<- any) {
	for {
		msg, err := c.Read()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				fmt.Fprintln(os.Stderr, "read:", err)
			}
			return
		}
		select {
		case events <- msg:
		default:
		}
		ts := time.Now().Format("15:04:05.000")
		switch m := msg.(type) {
		case node.Hello:
			fmt.Printf("%s hello: %q bt=%s protocol=%d codecs=0x%x queue=%d\n", ts, m.Name, m.BTAddr, m.Version, m.Codecs, m.QueueCapacity)
		case node.Status:
			if verbose {
				fmt.Printf("%s status: %s peer=%s mtu=%d queued=%d/%dms sent=%d underruns=%d dropped=%d heap=%d (min %d) up=%ds\n",
					ts, m.Link, m.Peer, m.MTU, m.QueuedPackets, m.QueuedUs/1000, m.SentPackets, m.Underruns, m.Dropped,
					m.FreeHeap, m.MinFreeHeap, m.UptimeS)
			}
		case node.Credit:
			if verbose {
				fmt.Printf("%s credit: queued=%d/%dms sent=%d underruns=%d\n", ts, m.QueuedPackets, m.QueuedUs/1000, m.SentPackets, m.Underruns)
			}
		case node.Adv:
			fmt.Printf("%s ble: %s type=%d rssi=%d company=%d name=%q\n", ts, m.Addr, m.AddrType, m.RSSI, m.Company, m.Name)
		case node.InquiryResult:
			fmt.Printf("%s found: %s class=0x%06x rssi=%d name=%q\n", ts, m.Addr, m.Class, m.RSSI, m.Name)
		case node.InquiryDone:
			fmt.Printf("%s inquiry done\n", ts)
		case node.Auth:
			fmt.Printf("%s pairing: %s status=%d name=%q\n", ts, m.Addr, m.Status, m.Name)
		case node.A2DPEvent:
			line := fmt.Sprintf("%s a2dp: %s %s mtu=%d detail=0x%02x", ts, node.EventName(m.Event), m.Addr, m.MTU, m.Detail)
			if m.Event == node.EvAudioConfig {
				if cfg, err := node.ParseCodecConfig(m.Info); err == nil {
					line += " " + cfg.String()
				} else {
					line += fmt.Sprintf(" info=% x (%v)", m.Info, err)
				}
			}
			fmt.Println(line)
		case node.RemoteCaps:
			for _, s := range m.SEPs {
				fmt.Printf("%s offers: seid %d %s (% x)\n", ts, s.SEID, node.CodecOf(s.Info), s.Info)
			}
		case node.AVRCPKey:
			state := "pressed"
			if m.Released {
				state = "released"
			}
			fmt.Printf("%s key: %s %s\n", ts, node.KeyName(m.Key), state)
		case node.Volume:
			fmt.Printf("%s volume: %d/127 origin=%d\n", ts, m.Volume, m.Origin)
		case node.Bonds:
			fmt.Printf("%s bonds: %v\n", ts, m)
		case node.Pong:
			fmt.Printf("%s pong %d\n", ts, m.Token)
		}
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "relayctl:", err)
	os.Exit(1)
}
