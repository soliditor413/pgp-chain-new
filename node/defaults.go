// Copyright 2016 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package node

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/nat"
	"github.com/ethereum/go-ethereum/rpc"
)

// The listening ports mirror pgp-chain's defaults, which is what operators of the
// existing network have in their runbooks: 20675 websocket, 20676 HTTP, 20678
// P2P, 20679 the PBFT network (see PbftNetPortFlag). Upstream geth's 8545/8546/
// 30303 would collide with whatever else runs on a host that already carries an
// ELA node, which is exactly the deployment shape here.
//
// The authenticated Engine API (8551) has no pgp-chain counterpart - that port
// arrived with the merge-era geth this fork is based on - so it keeps its
// upstream value rather than inventing a number in the 2067x series.
const (
	DefaultHTTPHost   = "localhost" // Default host interface for the HTTP RPC server
	DefaultHTTPPort   = 20676       // Default TCP port for the HTTP RPC server
	DefaultWSHost     = "localhost" // Default host interface for the websocket RPC server
	DefaultWSPort     = 20675       // Default TCP port for the websocket RPC server
	DefaultAuthHost   = "localhost" // Default host interface for the authenticated apis
	DefaultAuthPort   = 8551        // Default port for the authenticated apis
	DefaultListenPort = 20678       // Default port for the TCP listening address
	DefaultDiscPort   = 20678       // Default port for the UDP discovery address
)

const (
	// Engine API batch limits: these are not configurable by users, and should cover the
	// needs of all CLs.
	engineAPIBatchItemLimit         = 2000
	engineAPIBatchResponseSizeLimit = 250 * 1000 * 1000
	engineAPIBodyLimit              = 128 * 1024 * 1024
)

var (
	DefaultAuthCors    = []string{"localhost"} // Default cors domain for the authenticated apis
	DefaultAuthVhosts  = []string{"localhost"} // Default virtual hosts for the authenticated apis
	DefaultAuthOrigins = []string{"localhost"} // Default origins for the authenticated apis
	DefaultAuthPrefix  = ""                    // Default prefix for the authenticated apis
	DefaultAuthModules = []string{"eth", "engine"}
	DefaultTimeFormat  = "01-02|15:04:05.000"
)

// DefaultConfig contains reasonable default settings.
var DefaultConfig = Config{
	DataDir:              DefaultDataDir(),
	HTTPPort:             DefaultHTTPPort,
	AuthAddr:             DefaultAuthHost,
	AuthPort:             DefaultAuthPort,
	AuthVirtualHosts:     DefaultAuthVhosts,
	HTTPModules:          []string{"net", "web3"},
	HTTPVirtualHosts:     []string{"localhost"},
	HTTPTimeouts:         rpc.DefaultHTTPTimeouts,
	WSPort:               DefaultWSPort,
	WSModules:            []string{"net", "web3"},
	BatchRequestLimit:    1000,
	BatchResponseMaxSize: 25 * 1000 * 1000,
	GraphQLVirtualHosts:  []string{"localhost"},
	P2P: p2p.Config{
		// Built from the constant rather than spelled out: the two used to be
		// separate literals, so changing one left the effective default on the old
		// port and no test noticed.
		ListenAddr:    ":" + strconv.Itoa(DefaultListenPort),
		MaxPeers:      50,
		MaxPeersPerIP: 0, // by default, it will be same as MaxPeers
		NAT:           nat.Any(),
		DiscoveryV4:   true,
		DiscoveryV5:   true,
	},
	DBEngine: "", // Use whatever exists, will default to Pebble if non-existent and supported
	Instance: 1,
	LogConfig: &LogConfig{
		TimeFormat: &DefaultTimeFormat,
	},
}

// DefaultDataDir is the default data directory to use for the databases and other
// persistence requirements.
func DefaultDataDir() string {
	// Try to place the data folder in the user's home dir
	home := homeDir()
	if home != "" {
		switch runtime.GOOS {
		case "darwin":
			return filepath.Join(home, "Library", "PGP_Ethereum")
		case "windows":
			// We used to put everything in %HOME%\AppData\Roaming, but this caused
			// problems with non-typical setups. If this fallback location exists and
			// is non-empty, use it, otherwise DTRT and check %LOCALAPPDATA%.
			fallback := filepath.Join(home, "AppData", "Roaming", "PGP_Ethereum")
			appdata := windowsAppData()
			if appdata == "" || common.IsNonEmptyDir(fallback) {
				return fallback
			}
			return filepath.Join(appdata, "PGP_Ethereum")
		default:
			return filepath.Join(home, ".PGP_Ethereum")
		}
	}
	// As we cannot guess a stable location, return empty and handle later
	return ""
}

func windowsAppData() string {
	v := os.Getenv("LOCALAPPDATA")
	if v == "" {
		// Windows XP and below don't have LocalAppData. Crash here because
		// we don't support Windows XP and undefining the variable will cause
		// other issues.
		panic("environment variable LocalAppData is undefined")
	}
	return v
}

func homeDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	if usr, err := user.Current(); err == nil {
		return usr.HomeDir
	}
	return ""
}
