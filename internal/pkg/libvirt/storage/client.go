// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package storage manages host-local persistent directory pools.
package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"time"

	libvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
	"github.com/google/uuid"
	"libvirt.org/go/libvirtxml"
)

const operationTimeout = 5 * time.Second

// Connector opens bounded sessions against one modular storage daemon.
type Connector struct {
	socket string
	uri    string
}

// New configures the daemon endpoint without opening a connection.
func New(socket, uri string) *Connector {
	return &Connector{socket: socket, uri: uri}
}

// Open bounds dialing, the handshake, RPCs and graceful disconnect with one
// timeout context. Cancellation closes the transport because go-libvirt RPCs
// do not accept a context.
func (c *Connector) Open(ctx context.Context) (Client, error) {
	sessionCtx, cancel := context.WithTimeout(ctx, operationTimeout)

	conn, err := (&net.Dialer{}).DialContext(sessionCtx, "unix", c.socket)
	if err != nil {
		cancel()

		return nil, err
	}

	return c.OpenConn(sessionCtx, conn, cancel)
}

// OpenConn connects over an already-connected transport for this connector.
// It takes ownership of conn and cancel; ctx must bound the full session.
func (c *Connector) OpenConn(ctx context.Context, conn net.Conn, cancel context.CancelFunc) (Client, error) {
	rpc := libvirt.NewWithDialer(dialers.NewAlreadyConnected(conn))
	client := &client{conn: conn, rpc: rpc, cancel: cancel}

	client.stopClose = context.AfterFunc(ctx, func() { closeTransport(conn) })
	if err := rpc.ConnectToURI(libvirt.ConnectURI(c.uri)); err != nil {
		client.Close()

		return nil, err
	}

	// Only a completed handshake may send CONNECT_CLOSE during cleanup.
	client.disconnect = rpc.Disconnect

	return client, nil
}

// The URL is a fixed namespace identifier, not a network endpoint. Changing
// this identity changes pool UUIDs and breaks recognition of existing ownership.
var namespace = uuid.NewHash(sha256.New(), uuid.NameSpaceURL, []byte("https://talos.dev/storage-pools"), 8)

// UUID is stable across configuration changes and controller restarts, but
// deliberately differs across hosts. A fixed-width binary machine UUID makes
// the concatenation unambiguous, without relying on a separator convention.
// The caller must supply a validated, nonzero machine UUID.
//
// SHA-256 (version 8) rather than a SHA-1 version 5 UUID: SHA-1 panics under
// FIPS 140-only mode, and this runs at package init.
func UUID(machine uuid.UUID, name string) uuid.UUID {
	return uuid.NewHash(sha256.New(), namespace, append(machine[:], []byte(name)...), 8)
}

// Pool is the identity returned by libvirt, including inactive persistent pools.
type Pool struct {
	Name string
	UUID uuid.UUID
	// Target is the pool's directory as defined in libvirt; empty when unknown.
	Target string
}

// Volume is a file-backed volume requested within a pool.
type Volume struct {
	Name string
	// Capacity is the volume's virtual size in bytes.
	Capacity uint64
	// Format is libvirt's volume target format: "raw" or "qcow2".
	Format string
}

// Client is a bounded session; callers must Close it after each reconciliation.
// A fresh connection on each pass avoids retaining poisoned RPC connections.
type Client interface {
	Pools() ([]Pool, error)
	Ensure(Pool, string, func() error) error
	EnsureVolume(Pool, Volume) (string, error)
	Remove(Pool) error
	Stop(Pool) error
	Close()
}

// storageRPC is the set of libvirt calls this package makes. It deliberately lists no
// volume delete or resize call: a guest's data outlives its declaration, so there is no
// code path that can destroy it, by construction rather than by convention.
type storageRPC interface {
	ConnectListAllStoragePools(int32, libvirt.ConnectListAllStoragePoolsFlags) ([]libvirt.StoragePool, uint32, error)
	StoragePoolLookupByName(string) (libvirt.StoragePool, error)
	StoragePoolGetXMLDesc(libvirt.StoragePool, libvirt.StorageXMLFlags) (string, error)
	StoragePoolIsPersistent(libvirt.StoragePool) (int32, error)
	StoragePoolSetAutostart(libvirt.StoragePool, int32) error
	StoragePoolIsActive(libvirt.StoragePool) (int32, error)
	StoragePoolDestroy(libvirt.StoragePool) error
	StoragePoolDefineXML(string, uint32) (libvirt.StoragePool, error)
	StoragePoolCreate(libvirt.StoragePool, libvirt.StoragePoolCreateFlags) error
	StoragePoolUndefine(libvirt.StoragePool) error
	StoragePoolRefresh(libvirt.StoragePool, uint32) error
	StorageVolLookupByName(libvirt.StoragePool, string) (libvirt.StorageVol, error)
	StorageVolGetXMLDesc(libvirt.StorageVol, uint32) (string, error)
	StorageVolCreateXML(libvirt.StoragePool, string, libvirt.StorageVolCreateFlags) (libvirt.StorageVol, error)
	StorageVolGetPath(libvirt.StorageVol) (string, error)
}

type client struct {
	rpc        storageRPC
	conn       net.Conn
	cancel     context.CancelFunc
	stopClose  func() bool
	disconnect func() error
}

func (c *client) Close() {
	// Keep the session timer/cancellation callback armed while Disconnect waits
	// for its reply. On any error (or deadline) the transport is still closed.
	defer closeTransport(c.conn)
	defer c.cancel()
	defer c.stopClose()

	if c.disconnect != nil {
		if err := c.disconnect(); err != nil {
			return
		}
	}
}

func closeTransport(conn net.Conn) {
	// Cleanup is best-effort: a canceled or expired session is never reused,
	// and reporting a close failure cannot restore its outstanding RPCs.
	conn.Close() //nolint:errcheck
}

func (c *client) Pools() ([]Pool, error) {
	// Flags 0 lists every pool: an owned orphan may be active or inactive.
	pools, _, err := c.rpc.ConnectListAllStoragePools(1, 0)
	if err != nil {
		return nil, err
	}

	result := make([]Pool, 0, len(pools))

	for _, pool := range pools {
		target, err := c.target(pool)
		if err != nil {
			return nil, err
		}

		result = append(result, Pool{Name: pool.Name, UUID: uuid.UUID(pool.UUID), Target: target})
	}

	return result, nil
}

func (c *client) target(p libvirt.StoragePool) (string, error) {
	text, err := c.rpc.StoragePoolGetXMLDesc(p, 0)
	if err != nil {
		return "", err
	}

	var desc libvirtxml.StoragePool
	if err = desc.Unmarshal(text); err != nil {
		return "", err
	}

	if desc.Target == nil {
		return "", nil
	}

	return desc.Target.Path, nil
}

func (c *client) lookup(pool Pool) (libvirt.StoragePool, bool, error) {
	p, err := c.rpc.StoragePoolLookupByName(pool.Name)
	if err != nil {
		if rpcErr, ok := errors.AsType[libvirt.Error](err); ok && rpcErr.Code == uint32(libvirt.ErrNoStoragePool) {
			return libvirt.StoragePool{}, false, nil
		}

		return libvirt.StoragePool{}, false, err
	}

	if p.UUID != libvirt.UUID(pool.UUID) {
		return libvirt.StoragePool{}, false, fmt.Errorf("pool %q is not owned by this machine", pool.Name)
	}

	return p, true, nil
}

// Ensure disables libvirt autostart: only Talos may activate after the mount
// hold exists. prepare must create only the pool directory, on a held mount.
func (c *client) Ensure(pool Pool, target string, prepare func() error) error {
	p, exists, err := c.lookup(pool)
	if err != nil {
		return err
	}

	redefine := !exists
	if exists {
		redefine, err = c.prepareExisting(p, target)
		if err != nil {
			return err
		}
	}

	if err = prepare(); err != nil {
		return err
	}

	if redefine {
		p, err = c.define(pool, target)
		if err != nil {
			return err
		}
	}

	if err = c.rpc.StoragePoolSetAutostart(p, 0); err != nil {
		return err
	}

	active, err := c.rpc.StoragePoolIsActive(p)
	if err != nil {
		return err
	}

	if active == 0 {
		return c.rpc.StoragePoolCreate(p, 0)
	}

	return nil
}

// EnsureVolume creates vol in pool if it is absent and returns its host path.
//
// An existing volume is reused exactly as it lies. Nothing here truncates, resizes or
// reformats: a volume outlives the declaration that asked for it, so a disagreement with
// the requested capacity or format is reported rather than resolved.
func (c *client) EnsureVolume(pool Pool, vol Volume) (string, error) {
	p, exists, err := c.lookup(pool)
	if err != nil {
		return "", err
	}

	if !exists {
		return "", fmt.Errorf("storage pool %q is not defined", pool.Name)
	}

	v, found, err := c.volume(p, vol.Name)
	if err != nil {
		return "", err
	}

	if !found {
		v, err = c.createVolume(p, vol)
		if err != nil {
			return "", err
		}
	}

	if err = c.checkVolume(v, vol); err != nil {
		return "", err
	}

	return c.rpc.StorageVolGetPath(v)
}

func (c *client) volume(p libvirt.StoragePool, name string) (libvirt.StorageVol, bool, error) {
	v, err := c.rpc.StorageVolLookupByName(p, name)
	if err != nil {
		if rpcErr, ok := errors.AsType[libvirt.Error](err); ok && rpcErr.Code == uint32(libvirt.ErrNoStorageVol) {
			return libvirt.StorageVol{}, false, nil
		}

		return libvirt.StorageVol{}, false, err
	}

	return v, true, nil
}

// createVolume tolerates a volume that exists on disk but not in libvirt's pool cache:
// a file left by a previous boot is adopted after a refresh rather than failing forever.
func (c *client) createVolume(p libvirt.StoragePool, vol Volume) (libvirt.StorageVol, error) {
	desc := libvirtxml.StorageVolume{
		Type: "file",
		Name: vol.Name,
		Capacity: &libvirtxml.StorageVolumeSize{
			Unit:  "bytes",
			Value: vol.Capacity,
		},
		// Allocate nothing up front. libvirt's directory pool writes out a fully allocated
		// raw volume when allocation is omitted, which no disk-sized volume can finish
		// within operationTimeout.
		Allocation: &libvirtxml.StorageVolumeSize{
			Unit:  "bytes",
			Value: 0,
		},
		Target: &libvirtxml.StorageVolumeTarget{
			Format: &libvirtxml.StorageVolumeTargetFormat{
				Type: vol.Format,
			},
		},
	}

	text, err := desc.Marshal()
	if err != nil {
		return libvirt.StorageVol{}, err
	}

	v, err := c.rpc.StorageVolCreateXML(p, text, 0)
	if err == nil {
		return v, nil
	}

	if rpcErr, ok := errors.AsType[libvirt.Error](err); !ok || rpcErr.Code != uint32(libvirt.ErrStorageVolExist) {
		return libvirt.StorageVol{}, err
	}

	if err = c.rpc.StoragePoolRefresh(p, 0); err != nil {
		return libvirt.StorageVol{}, err
	}

	v, found, err := c.volume(p, vol.Name)
	if err != nil {
		return libvirt.StorageVol{}, err
	}

	if !found {
		return libvirt.StorageVol{}, fmt.Errorf("volume %q exists but is absent after a pool refresh", vol.Name)
	}

	return v, nil
}

// checkVolume refuses to hand back a volume that is not what was asked for, so a changed
// declaration surfaces as an error instead of silently attaching the wrong disk.
func (c *client) checkVolume(v libvirt.StorageVol, want Volume) error {
	text, err := c.rpc.StorageVolGetXMLDesc(v, 0)
	if err != nil {
		return err
	}

	var desc libvirtxml.StorageVolume

	if err = desc.Unmarshal(text); err != nil {
		return err
	}

	if desc.Capacity == nil || desc.Capacity.Value != want.Capacity {
		return fmt.Errorf("volume %q already exists with a different size; resizing is not supported", want.Name)
	}

	if desc.Target == nil || desc.Target.Format == nil || desc.Target.Format.Type != want.Format {
		return fmt.Errorf("volume %q already exists in a different format; reformatting is not supported", want.Name)
	}

	return nil
}

func (c *client) define(pool Pool, target string) (libvirt.StoragePool, error) {
	desc := libvirtxml.StoragePool{
		Type:   "dir",
		Name:   pool.Name,
		UUID:   pool.UUID.String(),
		Target: &libvirtxml.StoragePoolTarget{Path: target},
	}

	text, err := desc.Marshal()
	if err != nil {
		return libvirt.StoragePool{}, err
	}

	return c.rpc.StoragePoolDefineXML(text, 0)
}

// prepareExisting disables daemon autostart and stops a pool before redefining it.
func (c *client) prepareExisting(p libvirt.StoragePool, target string) (bool, error) {
	text, err := c.rpc.StoragePoolGetXMLDesc(p, 0)
	if err != nil {
		return false, err
	}

	var desc libvirtxml.StoragePool
	if err = desc.Unmarshal(text); err != nil {
		return false, err
	}

	if desc.Type != "dir" {
		return false, fmt.Errorf("pool %q is not a directory pool", p.Name)
	}

	persistent, err := c.rpc.StoragePoolIsPersistent(p)
	if err != nil {
		return false, err
	}

	if persistent != 0 {
		if err = c.rpc.StoragePoolSetAutostart(p, 0); err != nil {
			return false, err
		}
	}

	redefine := desc.Target == nil || desc.Target.Path != target || persistent == 0
	if redefine {
		return true, c.stop(p)
	}

	return false, nil
}

func (c *client) stop(p libvirt.StoragePool) error {
	active, err := c.rpc.StoragePoolIsActive(p)
	if err != nil {
		return err
	}

	if active != 0 {
		return c.rpc.StoragePoolDestroy(p)
	}

	return nil
}

// Stop leaves the persistent definition and all data intact.
func (c *client) Stop(pool Pool) error {
	p, exists, err := c.lookup(pool)
	if err != nil || !exists {
		return err
	}

	return c.stop(p)
}

// Remove always stops and undefines, regardless of pool contents. It never
// deletes a storage pool's data, enumerates files or calls StoragePoolDelete.
func (c *client) Remove(pool Pool) error {
	p, exists, err := c.lookup(pool)
	if err != nil || !exists {
		return err
	}

	if err = c.stop(p); err != nil {
		return err
	}

	return c.rpc.StoragePoolUndefine(p)
}
