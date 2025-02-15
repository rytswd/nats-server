// Copyright 2019-2020 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/minio/highwayhash"
	"github.com/nats-io/nats-server/v2/server/sysmem"
)

// JetStreamConfig determines this server's configuration.
// MaxMemory and MaxStore are in bytes.
type JetStreamConfig struct {
	MaxMemory int64
	MaxStore  int64
	StoreDir  string
}

// TODO(dlc) - need to track and rollup against server limits, etc.
type JetStreamAccountLimits struct {
	MaxMemory    int64 `json:"max_memory"`
	MaxStore     int64 `json:"max_storage"`
	MaxStreams   int   `json:"max_streams"`
	MaxConsumers int   `json:"max_consumers"`
}

// JetStreamAccountStats returns current statistics about the account's JetStream usage.
type JetStreamAccountStats struct {
	Memory  uint64                 `json:"memory"`
	Store   uint64                 `json:"storage"`
	Streams int                    `json:"streams"`
	Limits  JetStreamAccountLimits `json:"limits"`
}

// This is for internal accounting for JetStream for this server.
type jetStream struct {
	mu            sync.RWMutex
	srv           *Server
	config        JetStreamConfig
	cluster       *jetStreamCluster
	accounts      map[*Account]*jsAccount
	memReserved   int64
	storeReserved int64
}

// This represents a jetstream enabled account.
// Worth noting that we include the js ptr, this is because
// in general we want to be very efficient when receiving messages on
// and internal sub for a msgSet, so we will direct link to the msgSet
// and walk backwards as needed vs multiple hash lookups and locks, etc.
type jsAccount struct {
	mu            sync.RWMutex
	js            *jetStream
	account       *Account
	limits        JetStreamAccountLimits
	memReserved   int64
	memUsed       int64
	storeReserved int64
	storeUsed     int64
	storeDir      string
	streams       map[string]*Stream
	templates     map[string]*StreamTemplate
	store         TemplateStore
}

// EnableJetStream will enable JetStream support on this server with the given configuration.
// A nil configuration will dynamically choose the limits and temporary file storage directory.
// If this server is part of a cluster, a system account will need to be defined.
func (s *Server) EnableJetStream(config *JetStreamConfig) error {
	s.mu.Lock()
	if s.js != nil {
		s.mu.Unlock()
		return fmt.Errorf("jetstream already enabled")
	}
	s.Noticef("Starting JetStream")
	if config == nil || config.MaxMemory <= 0 || config.MaxStore <= 0 {
		var storeDir string
		var maxStore int64
		if config != nil {
			storeDir = config.StoreDir
			maxStore = config.MaxStore
		}
		config = s.dynJetStreamConfig(storeDir, maxStore)
		s.Debugf("JetStream creating dynamic configuration - %s memory, %s disk", FriendlyBytes(config.MaxMemory), FriendlyBytes(config.MaxStore))
	}
	// Copy, don't change callers version.
	cfg := *config
	if cfg.StoreDir == "" {
		cfg.StoreDir = filepath.Join(os.TempDir(), JetStreamStoreDir)
	}

	s.js = &jetStream{srv: s, config: cfg, accounts: make(map[*Account]*jsAccount)}
	s.mu.Unlock()

	// FIXME(dlc) - Allow memory only operation?
	if stat, err := os.Stat(cfg.StoreDir); os.IsNotExist(err) {
		if err := os.MkdirAll(cfg.StoreDir, 0755); err != nil {
			return fmt.Errorf("could not create storage directory - %v", err)
		}
	} else {
		// Make sure its a directory and that we can write to it.
		if stat == nil || !stat.IsDir() {
			return fmt.Errorf("storage directory is not a directory")
		}
		tmpfile, err := ioutil.TempFile(cfg.StoreDir, "_test_")
		if err != nil {
			return fmt.Errorf("storage directory is not writable")
		}
		os.Remove(tmpfile.Name())
	}

	// JetStream is an internal service so we need to make sure we have a system account.
	// This system account will export the JetStream service endpoints.
	if sacc := s.SystemAccount(); sacc == nil {
		s.SetDefaultSystemAccount()
	}

	s.Warnf("    _ ___ _____ ___ _____ ___ ___   _   __  __")
	s.Warnf(" _ | | __|_   _/ __|_   _| _ \\ __| /_\\ |  \\/  |")
	s.Warnf("| || | _|  | | \\__ \\ | | |   / _| / _ \\| |\\/| |")
	s.Warnf(" \\__/|___| |_| |___/ |_| |_|_\\___/_/ \\_\\_|  |_|")
	s.Warnf("")
	s.Warnf("               _         _")
	s.Warnf("              | |__  ___| |_ __ _")
	s.Warnf("              | '_ \\/ -_)  _/ _` |")
	s.Warnf("              |_.__/\\___|\\__\\__,_|")
	s.Warnf("")
	s.Warnf("         JetStream is a Beta feature")
	s.Warnf("    https://github.com/nats-io/jetstream")
	s.Noticef("")
	s.Noticef("----------- JETSTREAM -----------")
	s.Noticef("  Max Memory:      %s", FriendlyBytes(cfg.MaxMemory))
	s.Noticef("  Max Storage:     %s", FriendlyBytes(cfg.MaxStore))
	s.Noticef("  Store Directory: %q", cfg.StoreDir)
	s.Noticef("---------------------------------")

	// Setup our internal subscriptions.
	if err := s.setJetStreamExportSubs(); err != nil {
		return fmt.Errorf("Error setting up internal jetstream subscriptions: %v", err)
	}

	// Setup our internal system exports.
	sacc := s.SystemAccount()
	// FIXME(dlc) - Should we lock these down?
	s.Debugf("  Exports:")
	for _, export := range allJsExports {
		s.Debugf("     %s", export)
		if err := sacc.AddServiceExport(export, nil); err != nil {
			return fmt.Errorf("Error setting up jetstream service exports: %v", err)
		}
	}

	// If we are in clustered mode go ahead and start the meta controller.
	if !s.standAloneMode() {
		if err := s.enableJetStreamClustering(); err != nil {
			s.Errorf("Could not create JetStream cluster: %v", err)
			return err
		}
	}

	// If we have no configured accounts setup then setup imports on global account.
	if s.globalAccountOnly() {
		if err := s.GlobalAccount().EnableJetStream(nil); err != nil {
			return fmt.Errorf("Error enabling jetstream on the global account")
		}
	} else if err := s.configAllJetStreamAccounts(); err != nil {
		return fmt.Errorf("Error enabling jetstream on configured accounts: %v", err)
	}

	return nil
}

// enableAllJetStreamServiceImports turns on all service imports for jetstream for this account.
func (a *Account) enableAllJetStreamServiceImports() error {
	a.mu.RLock()
	s := a.srv
	a.mu.RUnlock()

	if s == nil {
		return fmt.Errorf("jetstream account not registered")
	}

	// In case the enabled import exists here.
	a.removeServiceImport(JSApiAccountInfo)

	sys := s.SystemAccount()
	for _, export := range allJsExports {
		if !a.serviceImportExists(sys, export) {
			if err := a.AddServiceImport(sys, export, _EMPTY_); err != nil {
				return fmt.Errorf("Error setting up jetstream service imports for account: %v", err)
			}
		}
	}
	return nil
}

// enableJetStreamEnabledServiceImportOnly will enable the single service import responder.
// Should we do them all regardless?
func (a *Account) enableJetStreamInfoServiceImportOnly() error {
	a.mu.RLock()
	s := a.srv
	a.mu.RUnlock()

	if s == nil {
		return fmt.Errorf("jetstream account not registered")
	}
	sys := s.SystemAccount()
	if err := a.AddServiceImport(sys, JSApiAccountInfo, _EMPTY_); err != nil {
		return fmt.Errorf("Error setting up jetstream service imports for account: %v", err)
	}
	return nil
}

func (s *Server) configJetStream(acc *Account) error {
	if acc.jsLimits != nil {
		// Check if already enabled. This can be during a reload.
		if acc.JetStreamEnabled() {
			if err := acc.enableAllJetStreamServiceImports(); err != nil {
				return err
			}
			if err := acc.UpdateJetStreamLimits(acc.jsLimits); err != nil {
				return err
			}
		} else if err := acc.EnableJetStream(acc.jsLimits); err != nil {
			return err
		}
		acc.jsLimits = nil
	} else if acc != s.SystemAccount() {
		if acc.JetStreamEnabled() {
			acc.DisableJetStream()
		}
		// We will setup basic service imports to respond to
		// requests if JS is enabled for this account.
		if err := acc.enableJetStreamInfoServiceImportOnly(); err != nil {
			return err
		}
	}
	return nil
}

// configAllJetStreamAccounts walk all configured accounts and turn on jetstream if requested.
func (s *Server) configAllJetStreamAccounts() error {
	// Check to see if system account has been enabled. We could arrive here via reload and
	// a non-default system account.
	if sacc := s.SystemAccount(); sacc != nil && !sacc.IsExportService(JSApiAccountInfo) {
		for _, export := range allJsExports {
			if err := sacc.AddServiceExport(export, nil); err != nil {
				return fmt.Errorf("Error setting up jetstream service exports: %v", err)
			}
		}
	}

	// Snapshot into our own list. Might not be needed.
	s.mu.Lock()
	// Bail if server not enabled. If it was enabled and a reload turns it off
	// that will be handled elsewhere.
	if s.js == nil {
		s.mu.Unlock()
		return nil
	}

	var jsAccounts []*Account

	s.accounts.Range(func(k, v interface{}) bool {
		jsAccounts = append(jsAccounts, v.(*Account))
		return true
	})
	s.mu.Unlock()

	// Process any jetstream enabled accounts here.
	for _, acc := range jsAccounts {
		if err := s.configJetStream(acc); err != nil {
			return err
		}
	}
	return nil
}

// JetStreamEnabled reports if jetstream is enabled.
func (s *Server) JetStreamEnabled() bool {
	s.mu.Lock()
	enabled := s.js != nil
	s.mu.Unlock()
	return enabled
}

// Shutdown jetstream for this server.
func (s *Server) shutdownJetStream() {
	s.mu.Lock()
	js := s.js
	s.mu.Unlock()

	if js == nil {
		return
	}

	var _jsa [512]*jsAccount
	jsas := _jsa[:0]

	js.mu.RLock()
	// Collect accounts.
	for _, jsa := range js.accounts {
		jsas = append(jsas, jsa)
	}
	js.mu.RUnlock()

	for _, jsa := range jsas {
		js.disableJetStream(jsa)
	}

	s.mu.Lock()
	s.js = nil
	s.mu.Unlock()

	js.mu.Lock()
	js.accounts = nil
	if cc := js.cluster; cc != nil {
		js.stopUpdatesSub()
		if cc.meta != nil {
			cc.meta.Stop()
			cc.meta = nil
		}
		if cc.c != nil {
			cc.c.closeConnection(ClientClosed)
			cc.c = nil
		}
	}
	js.mu.Unlock()
}

// JetStreamConfig will return the current config. Useful if the system
// created a dynamic configuration. A copy is returned.
func (s *Server) JetStreamConfig() *JetStreamConfig {
	var c *JetStreamConfig
	s.mu.Lock()
	if s.js != nil {
		copy := s.js.config
		c = &(copy)
	}
	s.mu.Unlock()
	return c
}

func (s *Server) StoreDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.js == nil {
		return _EMPTY_
	}
	return s.js.config.StoreDir
}

// JetStreamNumAccounts returns the number of enabled accounts this server is tracking.
func (s *Server) JetStreamNumAccounts() int {
	js := s.getJetStream()
	if js == nil {
		return 0
	}
	js.mu.Lock()
	defer js.mu.Unlock()
	return len(js.accounts)
}

// JetStreamReservedResources returns the reserved resources if JetStream is enabled.
func (s *Server) JetStreamReservedResources() (int64, int64, error) {
	js := s.getJetStream()
	if js == nil {
		return -1, -1, ErrJetStreamNotEnabled
	}
	js.mu.RLock()
	defer js.mu.RUnlock()
	return js.memReserved, js.storeReserved, nil
}

func (s *Server) getJetStream() *jetStream {
	s.mu.Lock()
	js := s.js
	s.mu.Unlock()
	return js
}

// EnableJetStream will enable JetStream on this account with the defined limits.
// This is a helper for JetStreamEnableAccount.
func (a *Account) EnableJetStream(limits *JetStreamAccountLimits) error {
	a.mu.RLock()
	s := a.srv
	a.mu.RUnlock()
	if s == nil {
		return fmt.Errorf("jetstream account not registered")
	}
	// FIXME(dlc) - cluster mode
	js := s.getJetStream()
	if js == nil {
		return ErrJetStreamNotEnabled
	}
	if s.SystemAccount() == a {
		return fmt.Errorf("jetstream can not be enabled on the system account")
	}

	// No limits means we dynamically set up limits.
	if limits == nil {
		limits = js.dynamicAccountLimits()
	}

	js.mu.Lock()
	// Check the limits against existing reservations.
	if _, ok := js.accounts[a]; ok {
		js.mu.Unlock()
		return fmt.Errorf("jetstream already enabled for account")
	}
	if err := js.sufficientResources(limits); err != nil {
		js.mu.Unlock()
		return err
	}
	jsa := &jsAccount{js: js, account: a, limits: *limits, streams: make(map[string]*Stream)}
	jsa.storeDir = path.Join(js.config.StoreDir, a.Name)
	js.accounts[a] = jsa
	js.reserveResources(limits)
	js.mu.Unlock()

	// Stamp inside account as well.
	a.mu.Lock()
	a.js = jsa
	a.mu.Unlock()

	// Create the proper imports here.
	if err := a.enableAllJetStreamServiceImports(); err != nil {
		return err
	}

	s.Debugf("Enabled JetStream for account %q", a.Name)
	s.Debugf("  Max Memory:      %s", FriendlyBytes(limits.MaxMemory))
	s.Debugf("  Max Storage:     %s", FriendlyBytes(limits.MaxStore))

	sdir := path.Join(jsa.storeDir, streamsDir)
	if _, err := os.Stat(sdir); os.IsNotExist(err) {
		if err := os.MkdirAll(sdir, 0755); err != nil {
			return fmt.Errorf("could not create storage streams directory - %v", err)
		}
	}

	// Restore any state here.
	s.Debugf("Recovering JetStream state for account %q", a.Name)

	// Check templates first since messsage sets will need proper ownership.
	// FIXME(dlc) - Make this consistent.
	tdir := path.Join(jsa.storeDir, tmplsDir)
	if stat, err := os.Stat(tdir); err == nil && stat.IsDir() {
		key := sha256.Sum256([]byte("templates"))
		hh, err := highwayhash.New64(key[:])
		if err != nil {
			return err
		}
		fis, _ := ioutil.ReadDir(tdir)
		for _, fi := range fis {
			metafile := path.Join(tdir, fi.Name(), JetStreamMetaFile)
			metasum := path.Join(tdir, fi.Name(), JetStreamMetaFileSum)
			buf, err := ioutil.ReadFile(metafile)
			if err != nil {
				s.Warnf("  Error reading StreamTemplate metafile %q: %v", metasum, err)
				continue
			}
			if _, err := os.Stat(metasum); os.IsNotExist(err) {
				s.Warnf("  Missing StreamTemplate checksum for %q", metasum)
				continue
			}
			sum, err := ioutil.ReadFile(metasum)
			if err != nil {
				s.Warnf("  Error reading StreamTemplate checksum %q: %v", metasum, err)
				continue
			}
			hh.Reset()
			hh.Write(buf)
			checksum := hex.EncodeToString(hh.Sum(nil))
			if checksum != string(sum) {
				s.Warnf("  StreamTemplate checksums do not match %q vs %q", sum, checksum)
				continue
			}
			var cfg StreamTemplateConfig
			if err := json.Unmarshal(buf, &cfg); err != nil {
				s.Warnf("  Error unmarshalling StreamTemplate metafile: %v", err)
				continue
			}
			cfg.Config.Name = _EMPTY_
			if _, err := a.AddStreamTemplate(&cfg); err != nil {
				s.Warnf("  Error recreating StreamTemplate %q: %v", cfg.Name, err)
				continue
			}
		}
	}

	// Now recover the streams.
	fis, _ := ioutil.ReadDir(sdir)
	for _, fi := range fis {
		mdir := path.Join(sdir, fi.Name())
		key := sha256.Sum256([]byte(fi.Name()))
		hh, err := highwayhash.New64(key[:])
		if err != nil {
			return err
		}
		metafile := path.Join(mdir, JetStreamMetaFile)
		metasum := path.Join(mdir, JetStreamMetaFileSum)
		if _, err := os.Stat(metafile); os.IsNotExist(err) {
			s.Warnf("  Missing Stream metafile for %q", metafile)
			continue
		}
		buf, err := ioutil.ReadFile(metafile)
		if err != nil {
			s.Warnf("  Error reading metafile %q: %v", metasum, err)
			continue
		}
		if _, err := os.Stat(metasum); os.IsNotExist(err) {
			s.Warnf("  Missing Stream checksum for %q", metasum)
			continue
		}
		sum, err := ioutil.ReadFile(metasum)
		if err != nil {
			s.Warnf("  Error reading Stream metafile checksum %q: %v", metasum, err)
			continue
		}
		hh.Write(buf)
		checksum := hex.EncodeToString(hh.Sum(nil))
		if checksum != string(sum) {
			s.Warnf("  Stream metafile checksums do not match %q vs %q", sum, checksum)
			continue
		}

		var cfg FileStreamInfo
		if err := json.Unmarshal(buf, &cfg); err != nil {
			s.Warnf("  Error unmarshalling Stream metafile: %v", err)
			continue
		}

		if cfg.Template != _EMPTY_ {
			if err := jsa.addStreamNameToTemplate(cfg.Template, cfg.Name); err != nil {
				s.Warnf("  Error adding Stream %q to Template %q: %v", cfg.Name, cfg.Template, err)
			}
		}
		mset, err := a.AddStream(&cfg.StreamConfig)
		if err != nil {
			s.Warnf("  Error recreating Stream %q: %v", cfg.Name, err)
			continue
		}
		if !cfg.Created.IsZero() {
			mset.setCreated(cfg.Created)
		}

		stats := mset.State()
		s.Noticef("  Restored %s messages for Stream %q", comma(int64(stats.Msgs)), fi.Name())

		// Now do the consumers.
		odir := path.Join(sdir, fi.Name(), consumerDir)
		ofis, _ := ioutil.ReadDir(odir)
		if len(ofis) > 0 {
			s.Noticef("  Recovering %d Consumers for Stream - %q", len(ofis), fi.Name())
		}
		for _, ofi := range ofis {
			metafile := path.Join(odir, ofi.Name(), JetStreamMetaFile)
			metasum := path.Join(odir, ofi.Name(), JetStreamMetaFileSum)
			if _, err := os.Stat(metafile); os.IsNotExist(err) {
				s.Warnf("    Missing Consumer Metafile %q", metafile)
				continue
			}
			buf, err := ioutil.ReadFile(metafile)
			if err != nil {
				s.Warnf("    Error reading consumer metafile %q: %v", metasum, err)
				continue
			}
			if _, err := os.Stat(metasum); os.IsNotExist(err) {
				s.Warnf("    Missing Consumer checksum for %q", metasum)
				continue
			}
			var cfg FileConsumerInfo
			if err := json.Unmarshal(buf, &cfg); err != nil {
				s.Warnf("    Error unmarshalling Consumer metafile: %v", err)
				continue
			}
			isEphemeral := !isDurableConsumer(&cfg.ConsumerConfig)
			if isEphemeral {
				// This is an ephermal consumer and this could fail on restart until
				// the consumer can reconnect. We will create it as a durable and switch it.
				cfg.ConsumerConfig.Durable = ofi.Name()
			}
			obs, err := mset.AddConsumer(&cfg.ConsumerConfig)
			if err != nil {
				s.Warnf("    Error adding Consumer: %v", err)
				continue
			}
			if isEphemeral {
				obs.switchToEphemeral()
			}
			if !cfg.Created.IsZero() {
				obs.setCreated(cfg.Created)
			}
			if err := obs.readStoredState(); err != nil {
				s.Warnf("    Error restoring Consumer state: %v", err)
			}
		}
	}

	// Make sure to cleanup and old remaining snapshots.
	os.RemoveAll(path.Join(jsa.storeDir, snapsDir))

	s.Debugf("JetStream state for account %q recovered", a.Name)

	return nil
}

// NumStreams will return how many streams we have.
func (a *Account) NumStreams() int {
	a.mu.RLock()
	jsa := a.js
	a.mu.RUnlock()
	if jsa == nil {
		return 0
	}
	jsa.mu.Lock()
	n := len(jsa.streams)
	jsa.mu.Unlock()
	return n
}

// Streams will return all known streams.
func (a *Account) Streams() []*Stream {
	return a.filteredStreams(_EMPTY_)
}

func (a *Account) filteredStreams(filter string) []*Stream {
	a.mu.RLock()
	jsa := a.js
	a.mu.RUnlock()

	if jsa == nil {
		return nil
	}

	jsa.mu.Lock()
	defer jsa.mu.Unlock()

	var msets []*Stream
	for _, mset := range jsa.streams {
		if filter != _EMPTY_ {
			for _, subj := range mset.config.Subjects {
				if SubjectsCollide(filter, subj) {
					msets = append(msets, mset)
					break
				}
			}
		} else {
			msets = append(msets, mset)
		}
	}

	return msets
}

// LookupStream will lookup a stream by name.
func (a *Account) LookupStream(name string) (*Stream, error) {
	a.mu.RLock()
	jsa := a.js
	a.mu.RUnlock()

	if jsa == nil {
		return nil, ErrJetStreamNotEnabled
	}
	jsa.mu.Lock()
	defer jsa.mu.Unlock()

	mset, ok := jsa.streams[name]
	if !ok {
		return nil, ErrJetStreamStreamNotFound
	}
	return mset, nil
}

// UpdateJetStreamLimits will update the account limits for a JetStream enabled account.
func (a *Account) UpdateJetStreamLimits(limits *JetStreamAccountLimits) error {
	a.mu.RLock()
	s := a.srv
	jsa := a.js
	a.mu.RUnlock()

	if s == nil {
		return fmt.Errorf("jetstream account not registered")
	}
	js := s.getJetStream()
	if js == nil {
		return ErrJetStreamNotEnabled
	}
	if jsa == nil {
		return ErrJetStreamNotEnabledForAccount
	}

	if limits == nil {
		limits = js.dynamicAccountLimits()
	}

	// Calculate the delta between what we have and what we want.
	jsa.mu.Lock()
	dl := diffCheckedLimits(&jsa.limits, limits)
	jsaLimits := jsa.limits
	jsa.mu.Unlock()

	js.mu.Lock()
	// Check the limits against existing reservations.
	if err := js.sufficientResources(&dl); err != nil {
		js.mu.Unlock()
		return err
	}
	// FIXME(dlc) - If we drop and are over the max on memory or store, do we delete??
	js.releaseResources(&jsaLimits)
	js.reserveResources(limits)
	js.mu.Unlock()

	// Update
	jsa.mu.Lock()
	jsa.limits = *limits
	jsa.mu.Unlock()

	return nil
}

func diffCheckedLimits(a, b *JetStreamAccountLimits) JetStreamAccountLimits {
	return JetStreamAccountLimits{
		MaxMemory: b.MaxMemory - a.MaxMemory,
		MaxStore:  b.MaxStore - a.MaxStore,
	}
}

// JetStreamUsage reports on JetStream usage and limits for an account.
func (a *Account) JetStreamUsage() JetStreamAccountStats {
	a.mu.RLock()
	jsa := a.js
	a.mu.RUnlock()

	var stats JetStreamAccountStats
	if jsa != nil {
		jsa.mu.Lock()
		stats.Memory = uint64(jsa.memUsed)
		stats.Store = uint64(jsa.storeUsed)
		stats.Streams = len(jsa.streams)
		stats.Limits = jsa.limits
		jsa.mu.Unlock()
	}
	return stats
}

// DisableJetStream will disable JetStream for this account.
func (a *Account) DisableJetStream() error {
	a.mu.Lock()
	s := a.srv
	a.js = nil
	a.mu.Unlock()

	if s == nil {
		return fmt.Errorf("jetstream account not registered")
	}

	js := s.getJetStream()
	if js == nil {
		return ErrJetStreamNotEnabled
	}

	// Remove service imports.
	for _, export := range allJsExports {
		a.removeServiceImport(export)
	}

	return js.disableJetStream(js.lookupAccount(a))
}

// Disable JetStream for the account.
func (js *jetStream) disableJetStream(jsa *jsAccount) error {
	if jsa == nil {
		return ErrJetStreamNotEnabledForAccount
	}

	js.mu.Lock()
	delete(js.accounts, jsa.account)
	js.releaseResources(&jsa.limits)
	js.mu.Unlock()

	jsa.delete()

	return nil
}

// JetStreamEnabled is a helper to determine if jetstream is enabled for an account.
func (a *Account) JetStreamEnabled() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	enabled := a.js != nil
	a.mu.RUnlock()
	return enabled
}

// Updates accounting on in use memory and storage.
func (jsa *jsAccount) updateUsage(storeType StorageType, delta int64) {
	// TODO(dlc) - atomics? snapshot limits?
	jsa.mu.Lock()
	if storeType == MemoryStorage {
		jsa.memUsed += delta
	} else {
		jsa.storeUsed += delta
	}
	jsa.mu.Unlock()
}

func (jsa *jsAccount) limitsExceeded(storeType StorageType) bool {
	var exceeded bool
	jsa.mu.Lock()
	if storeType == MemoryStorage {
		if jsa.limits.MaxMemory > 0 && jsa.memUsed > jsa.limits.MaxMemory {
			exceeded = true
		}
	} else {
		if jsa.limits.MaxStore > 0 && jsa.storeUsed > jsa.limits.MaxStore {
			exceeded = true
		}
	}
	jsa.mu.Unlock()
	return exceeded
}

// Check if a new proposed msg set while exceed our account limits.
// Lock should be held.
func (jsa *jsAccount) checkLimits(config *StreamConfig) error {
	if jsa.limits.MaxStreams > 0 && len(jsa.streams) >= jsa.limits.MaxStreams {
		return fmt.Errorf("maximum number of streams reached")
	}
	// Check MaxConsumers
	if config.MaxConsumers > 0 && jsa.limits.MaxConsumers > 0 && config.MaxConsumers > jsa.limits.MaxConsumers {
		return fmt.Errorf("maximum consumers exceeds account limit")
	}

	// Check storage, memory or disk.
	if config.MaxBytes > 0 {
		return jsa.checkBytesLimits(config.MaxBytes*int64(config.Replicas), config.Storage)
	}
	return nil
}

// Check if additional bytes will exceed our account limits.
// This should account for replicas.
// Lock should be held.
func (jsa *jsAccount) checkBytesLimits(addBytes int64, storage StorageType) error {
	switch storage {
	case MemoryStorage:
		if jsa.memReserved+addBytes > jsa.limits.MaxMemory {
			return fmt.Errorf("insufficient memory resources available")
		}
	case FileStorage:
		if jsa.storeReserved+addBytes > jsa.limits.MaxStore {
			return fmt.Errorf("insufficient storage resources available")
		}
	}
	return nil
}

func (jsa *jsAccount) acc() *Account {
	jsa.mu.RLock()
	acc := jsa.account
	jsa.mu.RUnlock()
	return acc
}

// Delete the JetStream resources.
func (jsa *jsAccount) delete() {
	var streams []*Stream
	var ts []string

	jsa.mu.Lock()
	for _, ms := range jsa.streams {
		streams = append(streams, ms)
	}
	acc := jsa.account
	for _, t := range jsa.templates {
		ts = append(ts, t.Name)
	}
	jsa.templates = nil
	jsa.mu.Unlock()

	for _, ms := range streams {
		ms.stop(false)
	}

	for _, t := range ts {
		acc.DeleteStreamTemplate(t)
	}
}

// Lookup the jetstream account for a given account.
func (js *jetStream) lookupAccount(a *Account) *jsAccount {
	js.mu.RLock()
	jsa := js.accounts[a]
	js.mu.RUnlock()
	return jsa
}

// Will dynamically create limits for this account.
func (js *jetStream) dynamicAccountLimits() *JetStreamAccountLimits {
	js.mu.RLock()
	// For now used all resources. Mostly meant for $G in non-account mode.
	limits := &JetStreamAccountLimits{js.config.MaxMemory, js.config.MaxStore, -1, -1}
	js.mu.RUnlock()
	return limits
}

// Check to see if we have enough system resources for this account.
// Lock should be held.
func (js *jetStream) sufficientResources(limits *JetStreamAccountLimits) error {
	if limits == nil {
		return nil
	}
	if js.memReserved+limits.MaxMemory > js.config.MaxMemory {
		return fmt.Errorf("insufficient memory resources available")
	}
	if js.storeReserved+limits.MaxStore > js.config.MaxStore {
		return fmt.Errorf("insufficient storage resources available")
	}
	return nil
}

// This will (blindly) reserve the respources requested.
// Lock should be held.
func (js *jetStream) reserveResources(limits *JetStreamAccountLimits) error {
	if limits == nil {
		return nil
	}
	if limits.MaxMemory > 0 {
		js.memReserved += limits.MaxMemory
	}
	if limits.MaxStore > 0 {
		js.storeReserved += limits.MaxStore
	}
	return nil
}

// Lock should be held.
func (js *jetStream) releaseResources(limits *JetStreamAccountLimits) error {
	if limits == nil {
		return nil
	}
	if limits.MaxMemory > 0 {
		js.memReserved -= limits.MaxMemory
	}
	if limits.MaxStore > 0 {
		js.storeReserved -= limits.MaxStore
	}
	return nil
}

// Will clear the resource reservations. Mostly for reload of a config.
func (js *jetStream) clearResources() {
	if js == nil {
		return
	}
	js.mu.Lock()
	js.memReserved = 0
	js.storeReserved = 0
	js.mu.Unlock()
}

const (
	// JetStreamStoreDir is the prefix we use.
	JetStreamStoreDir = "jetstream"
	// JetStreamMaxStoreDefault is the default disk storage limit. 1TB
	JetStreamMaxStoreDefault = 1024 * 1024 * 1024 * 1024
	// JetStreamMaxMemDefault is only used when we can't determine system memory. 256MB
	JetStreamMaxMemDefault = 1024 * 1024 * 256
)

// Dynamically create a config with a tmp based directory (repeatable) and 75% of system memory.
func (s *Server) dynJetStreamConfig(storeDir string, maxStore int64) *JetStreamConfig {
	jsc := &JetStreamConfig{}
	if storeDir != _EMPTY_ {
		jsc.StoreDir = filepath.Join(storeDir, JetStreamStoreDir)
	} else {
		// Create one in tmp directory, but make it consistent for restarts.
		jsc.StoreDir = filepath.Join(os.TempDir(), "nats", JetStreamStoreDir)
	}

	if maxStore > 0 {
		jsc.MaxStore = maxStore
	} else {
		jsc.MaxStore = diskAvailable(jsc.StoreDir)
	}
	// Estimate to 75% of total memory if we can determine system memory.
	if sysMem := sysmem.Memory(); sysMem > 0 {
		jsc.MaxMemory = sysMem / 4 * 3
	} else {
		jsc.MaxMemory = JetStreamMaxMemDefault
	}
	return jsc
}

// Helper function.
func (a *Account) checkForJetStream() (*Server, *jsAccount, error) {
	a.mu.RLock()
	s := a.srv
	jsa := a.js
	a.mu.RUnlock()

	if s == nil || jsa == nil {
		return nil, nil, ErrJetStreamNotEnabledForAccount
	}

	return s, jsa, nil
}

// StreamTemplateConfig allows a configuration to auto-create streams based on this template when a message
// is received that matches. Each new stream will use the config as the template config to create them.
type StreamTemplateConfig struct {
	Name       string        `json:"name"`
	Config     *StreamConfig `json:"config"`
	MaxStreams uint32        `json:"max_streams"`
}

// StreamTemplateInfo
type StreamTemplateInfo struct {
	Config  *StreamTemplateConfig `json:"config"`
	Streams []string              `json:"streams"`
}

// StreamTemplate
type StreamTemplate struct {
	mu  sync.Mutex
	tc  *client
	jsa *jsAccount
	*StreamTemplateConfig
	streams []string
}

func (t *StreamTemplateConfig) deepCopy() *StreamTemplateConfig {
	copy := *t
	cfg := *t.Config
	copy.Config = &cfg
	return &copy
}

// AddStreamTemplate will add a stream template to this account that allows auto-creation of streams.
func (a *Account) AddStreamTemplate(tc *StreamTemplateConfig) (*StreamTemplate, error) {
	s, jsa, err := a.checkForJetStream()
	if err != nil {
		return nil, err
	}
	if tc.Config.Name != "" {
		return nil, fmt.Errorf("template config name should be empty")
	}
	if len(tc.Name) > JSMaxNameLen {
		return nil, fmt.Errorf("template name is too long, maximum allowed is %d", JSMaxNameLen)
	}

	// FIXME(dlc) - Hacky
	tcopy := tc.deepCopy()
	tcopy.Config.Name = "_"
	cfg, err := checkStreamCfg(tcopy.Config)
	if err != nil {
		return nil, err
	}
	tcopy.Config = &cfg
	t := &StreamTemplate{
		StreamTemplateConfig: tcopy,
		tc:                   s.createInternalJetStreamClient(),
		jsa:                  jsa,
	}
	t.tc.registerWithAccount(a)

	jsa.mu.Lock()
	if jsa.templates == nil {
		jsa.templates = make(map[string]*StreamTemplate)
		// Create the appropriate store
		if cfg.Storage == FileStorage {
			jsa.store = newTemplateFileStore(jsa.storeDir)
		} else {
			jsa.store = newTemplateMemStore()
		}
	} else if _, ok := jsa.templates[tcopy.Name]; ok {
		jsa.mu.Unlock()
		return nil, fmt.Errorf("template with name %q already exists", tcopy.Name)
	}
	jsa.templates[tcopy.Name] = t
	jsa.mu.Unlock()

	// FIXME(dlc) - we can not overlap subjects between templates. Need to have test.

	// Setup the internal subscriptions to trap the messages.
	if err := t.createTemplateSubscriptions(); err != nil {
		return nil, err
	}
	if err := jsa.store.Store(t); err != nil {
		t.Delete()
		return nil, err
	}
	return t, nil
}

func (t *StreamTemplate) createTemplateSubscriptions() error {
	if t == nil {
		return fmt.Errorf("no template")
	}
	if t.tc == nil {
		return fmt.Errorf("template not enabled")
	}
	c := t.tc
	if !c.srv.eventsEnabled() {
		return ErrNoSysAccount
	}
	sid := 1
	for _, subject := range t.Config.Subjects {
		// Now create the subscription
		if _, err := c.processSub([]byte(subject), nil, []byte(strconv.Itoa(sid)), t.processInboundTemplateMsg, false); err != nil {
			c.acc.DeleteStreamTemplate(t.Name)
			return err
		}
		sid++
	}
	return nil
}

func (t *StreamTemplate) processInboundTemplateMsg(_ *subscription, pc *client, subject, reply string, msg []byte) {
	if t == nil || t.jsa == nil {
		return
	}
	jsa := t.jsa
	cn := CanonicalName(subject)

	jsa.mu.Lock()
	// If we already are registered then we can just return here.
	if _, ok := jsa.streams[cn]; ok {
		jsa.mu.Unlock()
		return
	}
	acc := jsa.account
	jsa.mu.Unlock()

	// Check if we are at the maximum and grab some variables.
	t.mu.Lock()
	c := t.tc
	cfg := *t.Config
	cfg.Template = t.Name
	atLimit := len(t.streams) >= int(t.MaxStreams)
	if !atLimit {
		t.streams = append(t.streams, cn)
	}
	t.mu.Unlock()

	if atLimit {
		c.Warnf("JetStream could not create stream for account %q on subject %q, at limit", acc.Name, subject)
		return
	}

	// We need to create the stream here.
	// Change the config from the template and only use literal subject.
	cfg.Name = cn
	cfg.Subjects = []string{subject}
	mset, err := acc.AddStream(&cfg)
	if err != nil {
		acc.validateStreams(t)
		c.Warnf("JetStream could not create stream for account %q on subject %q", acc.Name, subject)
		return
	}

	// Process this message directly by invoking mset.
	mset.processInboundJetStreamMsg(nil, pc, subject, reply, msg)
}

// LookupStreamTemplate looks up the names stream template.
func (a *Account) LookupStreamTemplate(name string) (*StreamTemplate, error) {
	_, jsa, err := a.checkForJetStream()
	if err != nil {
		return nil, err
	}
	jsa.mu.Lock()
	defer jsa.mu.Unlock()
	if jsa.templates == nil {
		return nil, fmt.Errorf("template not found")
	}
	t, ok := jsa.templates[name]
	if !ok {
		return nil, fmt.Errorf("template not found")
	}
	return t, nil
}

// This function will check all named streams and make sure they are valid.
func (a *Account) validateStreams(t *StreamTemplate) {
	t.mu.Lock()
	var vstreams []string
	for _, sname := range t.streams {
		if _, err := a.LookupStream(sname); err == nil {
			vstreams = append(vstreams, sname)
		}
	}
	t.streams = vstreams
	t.mu.Unlock()
}

func (t *StreamTemplate) Delete() error {
	if t == nil {
		return fmt.Errorf("nil stream template")
	}

	t.mu.Lock()
	jsa := t.jsa
	c := t.tc
	t.tc = nil
	defer func() {
		if c != nil {
			c.closeConnection(ClientClosed)
		}
	}()
	t.mu.Unlock()

	if jsa == nil {
		return ErrJetStreamNotEnabled
	}

	jsa.mu.Lock()
	if jsa.templates == nil {
		jsa.mu.Unlock()
		return fmt.Errorf("template not found")
	}
	if _, ok := jsa.templates[t.Name]; !ok {
		jsa.mu.Unlock()
		return fmt.Errorf("template not found")
	}
	delete(jsa.templates, t.Name)
	acc := jsa.account
	jsa.mu.Unlock()

	// Remove streams associated with this template.
	var streams []*Stream
	t.mu.Lock()
	for _, name := range t.streams {
		if mset, err := acc.LookupStream(name); err == nil {
			streams = append(streams, mset)
		}
	}
	t.mu.Unlock()

	if jsa.store != nil {
		if err := jsa.store.Delete(t); err != nil {
			return fmt.Errorf("error deleting template from store: %v", err)
		}
	}

	var lastErr error
	for _, mset := range streams {
		if err := mset.Delete(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func (a *Account) DeleteStreamTemplate(name string) error {
	t, err := a.LookupStreamTemplate(name)
	if err != nil {
		return err
	}
	return t.Delete()
}

func (a *Account) Templates() []*StreamTemplate {
	var ts []*StreamTemplate
	_, jsa, err := a.checkForJetStream()
	if err != nil {
		return nil
	}

	jsa.mu.Lock()
	for _, t := range jsa.templates {
		// FIXME(dlc) - Copy?
		ts = append(ts, t)
	}
	jsa.mu.Unlock()

	return ts
}

// Will add a stream to a template, this is for recovery.
func (jsa *jsAccount) addStreamNameToTemplate(tname, mname string) error {
	if jsa.templates == nil {
		return fmt.Errorf("template not found")
	}
	t, ok := jsa.templates[tname]
	if !ok {
		return fmt.Errorf("template not found")
	}
	// We found template.
	t.mu.Lock()
	t.streams = append(t.streams, mname)
	t.mu.Unlock()
	return nil
}

// This will check if a template owns this stream.
// jsAccount lock should be held
func (jsa *jsAccount) checkTemplateOwnership(tname, sname string) bool {
	if jsa.templates == nil {
		return false
	}
	t, ok := jsa.templates[tname]
	if !ok {
		return false
	}
	// We found template, make sure we are in streams.
	for _, streamName := range t.streams {
		if sname == streamName {
			return true
		}
	}
	return false
}

// FriendlyBytes returns a string with the given bytes int64
// represented as a size, such as 1KB, 10MB, etc...
func FriendlyBytes(bytes int64) string {
	fbytes := float64(bytes)
	base := 1024
	pre := []string{"K", "M", "G", "T", "P", "E"}
	if fbytes < float64(base) {
		return fmt.Sprintf("%v B", fbytes)
	}
	exp := int(math.Log(fbytes) / math.Log(float64(base)))
	index := exp - 1
	return fmt.Sprintf("%.2f %sB", fbytes/math.Pow(float64(base), float64(exp)), pre[index])
}

func isValidName(name string) bool {
	if name == "" {
		return false
	}
	return !strings.ContainsAny(name, ".*>")
}

// CanonicalName will replace all token separators '.' with '_'.
// This can be used when naming streams or consumers with multi-token subjects.
func CanonicalName(name string) string {
	return strings.ReplaceAll(name, ".", "_")
}
