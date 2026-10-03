package bridge

import (
	"context"
	"fmt"
	"time"

	"github.com/sl1288/a2dp-relay-bridge/internal/config"
	"github.com/sl1288/a2dp-relay-bridge/internal/node"
)

// BLE pairing learns a headphone's identity resolving key (IRK). Headphones
// that advertise without a name and with rotating private addresses can only
// be recognized with it. A node connects to the chosen advertiser over BLE and
// pairs; the headphone usually has to be in pairing mode.

type blePairJob struct {
	node   *nodeSession
	addr   node.Addr
	result chan node.BLEPair
}

func (b *Bridge) onBLEPair(n *nodeSession, r node.BLEPair) {
	b.mu.Lock()
	job := b.blePairing
	b.mu.Unlock()
	if job != nil && job.node == n && job.addr == r.Addr {
		select {
		case job.result <- r:
		default:
		}
	}
}

// CodedError is an error the web interface translates by its code
// ("err_" + Code), filling in Args.
type CodedError struct {
	Code string
	Args []string
	Msg  string // English text for logs and other clients
}

func (e *CodedError) Error() string { return e.Msg }

// blePairError explains a failed BLE pairing.
func blePairError(r node.BLEPair) error {
	code := fmt.Sprintf("0x%02X", r.Reason)
	switch r.Status {
	case node.BLEPairBusy:
		return &CodedError{"ble_pair_busy", nil, "the node is busy with another BLE pairing"}
	case node.BLEPairConnectFailed:
		return &CodedError{"ble_pair_connect", []string{code},
			"no BLE connection (code " + code + "); the address may have changed: scan again and retry"}
	case node.BLEPairAuthFailed:
		// Bluedroid reports SMP reasons offset by BTA_DM_AUTH_FAIL_BASE (0x4D);
		// 0x16 is SMP_RSP_TIMEOUT: the headphone never answered the request.
		if r.Reason == 0x4D+0x16 {
			return &CodedError{"ble_pair_no_response", nil,
				"the headphone accepted the BLE connection but did not answer the pairing request"}
		}
		return &CodedError{"ble_pair_auth", []string{code},
			"the headphone refused BLE pairing (code " + code + "); put it into pairing mode and retry"}
	case node.BLEPairNoIRK:
		return &CodedError{"ble_pair_no_irk", nil, "paired, but the headphone sent no identity key"}
	case node.BLEPairTimeout:
		return &CodedError{"ble_pair_timeout", nil, "no answer from the headphone; put it into pairing mode and retry"}
	case node.BLEPairUnsupported:
		return &CodedError{"ble_pair_unsupported", nil, "the node firmware has no BLE pairing (ble_pairing: false)"}
	}
	return fmt.Errorf("BLE pairing failed (status %d)", r.Status)
}

// BLEPair pairs node nodeID over BLE with the advertiser at addr and stores
// the identity key it learns for headphone id.
func (b *Bridge) BLEPair(ctx context.Context, id, nodeID, addrStr string, addrType uint8) (config.Headphone, error) {
	h := b.headphoneByID(id)
	if h == nil {
		return config.Headphone{}, fmt.Errorf("unknown headphone %q", id)
	}
	n := b.nodeByID(nodeID)
	if n == nil {
		return config.Headphone{}, fmt.Errorf("unknown node %q", nodeID)
	}
	addr, err := node.ParseAddr(addrStr)
	if err != nil {
		return config.Headphone{}, err
	}
	c, err := n.getConn()
	if err != nil {
		return config.Headphone{}, err
	}
	job := &blePairJob{node: n, addr: addr, result: make(chan node.BLEPair, 1)}
	b.mu.Lock()
	if b.blePairing != nil {
		b.mu.Unlock()
		return config.Headphone{}, &CodedError{"ble_pair_busy", nil, "another BLE pairing is in progress"}
	}
	b.blePairing = job
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.blePairing = nil
		b.mu.Unlock()
	}()

	h.log.Info("BLE pairing to learn the identity key", "addr", addr, "node", n.name())
	if err := c.BLEPair(addr, addrType); err != nil {
		return config.Headphone{}, err
	}
	// The node gives up after 40 s.
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var r node.BLEPair
	select {
	case r = <-job.result:
	case <-ctx.Done():
		return config.Headphone{}, &CodedError{"ble_pair_timeout", nil, "no answer from the node"}
	}
	if r.Status != node.BLEPairOK {
		err := blePairError(r)
		h.log.Warn("BLE pairing failed", "err", err)
		return config.Headphone{}, err
	}
	identity := ""
	if !r.Identity.IsZero() {
		identity = r.Identity.String()
	}
	cfg, err := b.store.Update(id, false, func(hc *config.Headphone) {
		hc.BLEIRK, hc.BLEIdentity, hc.BLEIRKUnverified = r.IRK.String(), identity, false
	})
	if err != nil {
		return cfg, err
	}
	h.setConfig(cfg)
	h.log.Info("learned the BLE identity key", "identity", identity, "resolves_used_address", r.IRK.Resolves(addr))
	b.refreshScans()
	b.notify()
	return cfg, nil
}

// SetBLEIRK stores an identity key the user copied from another system (for
// example a computer the headphone is paired with). Systems store the key in
// either byte order: the order that resolves an address seen in the last BLE
// scan wins. Without such an address both orders are tried until one resolves.
// It returns the address that confirmed the key, if any.
func (b *Bridge) SetBLEIRK(id, irkStr string) (string, error) {
	k, err := node.ParseIRK(irkStr)
	if err != nil {
		return "", &CodedError{"irk_invalid", nil, err.Error()}
	}
	if b.headphoneByID(id) == nil {
		return "", fmt.Errorf("unknown headphone %q", id)
	}
	irk, verified, matched := k, false, ""
	for _, n := range b.nodes {
		for _, s := range n.seenBLE() {
			a, err := node.ParseAddr(s.Addr)
			if err != nil || !node.IsRPA(a, s.AddrType) || verified {
				continue
			}
			if k.Resolves(a) {
				irk, verified, matched = k, true, s.Addr
			} else if r := k.Reversed(); r.Resolves(a) {
				irk, verified, matched = r, true, s.Addr
			}
		}
	}
	cfg, err := b.store.Update(id, false, func(hc *config.Headphone) {
		hc.BLEIRK, hc.BLEIdentity, hc.BLEIRKUnverified = irk.String(), "", !verified
	})
	if err != nil {
		return "", err
	}
	h := b.headphoneByID(id)
	h.setConfig(cfg)
	h.log.Info("identity key entered", "verified_by", matched)
	b.refreshScans()
	b.notify()
	return matched, nil
}

// confirmIRK settles the byte order of an unverified IRK once it resolved an
// advertisement.
func (b *Bridge) confirmIRK(h *headphone, reversed bool) {
	cfg, err := b.store.Update(h.id(), false, func(hc *config.Headphone) {
		if k, err := node.ParseIRK(hc.BLEIRK); err == nil && reversed {
			hc.BLEIRK = k.Reversed().String()
		}
		hc.BLEIRKUnverified = false
	})
	if err != nil {
		h.log.Warn("saving identity key failed", "err", err)
		return
	}
	h.setConfig(cfg)
	h.log.Info("identity key confirmed by an advertisement", "reversed", reversed)
	b.refreshScans()
	b.notify()
}

// ClearBLEIRK forgets the identity key of headphone id.
func (b *Bridge) ClearBLEIRK(id string) error {
	cfg, err := b.store.Update(id, false, func(hc *config.Headphone) {
		hc.BLEIRK, hc.BLEIdentity, hc.BLEIRKUnverified = "", "", false
	})
	if err != nil {
		return err
	}
	if h := b.headphoneByID(id); h != nil {
		h.setConfig(cfg)
	}
	b.refreshScans()
	b.notify()
	return nil
}
