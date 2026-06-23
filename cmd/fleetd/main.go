// fleetd is the Alien Fleet daemon — one binary, multiple roles (AX1):
//
//	fleetd gateway  — regional PoP + Phase-0 testnet chain-sim
//	fleetd node     — Class O serving node (OpenAI-compatible backend, GPU-pinned)
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"alien.org/fleet/internal/gateway"
	"alien.org/fleet/internal/identity"
	"alien.org/fleet/internal/ledger"
	"alien.org/fleet/internal/node"
	"alien.org/fleet/internal/wire"
)

func usage() {
	fmt.Fprintf(os.Stderr, `fleetd — Alien Fleet daemon (one binary, multiple roles, AX1)

Usage:
  fleetd chain [flags]       run the Phase-0 testnet chain-sim (stands in for
                             the L2 rollup of §14: registry, payments, faucet)
  fleetd gateway [flags]     run a gateway PoP (stateless; any number may run)
  fleetd node -config <file> run a Class O serving node (permissionless join)

Chain flags:
  -listen addr        listen address (default 127.0.0.1:9500)
  -state file         ledger state snapshot path
  -shadow duration    SHADOW probation window (testnet default 5s; mainnet 24h)
  -dev-faucet         allow faucet claims without Agent ID (Phase-0 testnet)
  -sso url            Alien SSO issuer for Agent ID verification
  -faucet-grant n     trial credit in FLEET (default 50)
  -embed-relay        also serve /v1/relay (single-process testnet convenience)

Gateway flags:
  -listen addr        listen address (default 127.0.0.1:9000)
  -chain url          chain endpoint (default http://127.0.0.1:9500)
  -name s             gateway name for logs
`)
	os.Exit(2)
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "chain":
		runChain(os.Args[2:])
	case "gateway":
		runGateway(os.Args[2:])
	case "node":
		runNode(os.Args[2:])
	default:
		usage()
	}
}

func runChain(args []string) {
	fs := flag.NewFlagSet("chain", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:9500", "listen address")
	state := fs.String("state", "", "ledger state snapshot path")
	shadow := fs.Duration("shadow", 5*time.Second, "SHADOW probation window")
	devFaucet := fs.Bool("dev-faucet", false, "allow faucet claims without Agent ID (Phase-0)")
	sso := fs.String("sso", "https://sso.alien-api.com", "Alien SSO issuer (Agent ID)")
	faucetGrant := fs.Uint64("faucet-grant", 50, "faucet trial credit, whole FLEET")
	nodeBond := fs.Float64("node-bond", 5, "earned-escrow bond target, FLEET (§8.1)")
	embedRelay := fs.Bool("embed-relay", false, "also serve /v1/relay from this process")
	_ = fs.Parse(args)

	params := ledger.DefaultParams()
	params.FaucetGrant = *faucetGrant * wire.NanoFLEET
	params.NodeBond = uint64(*nodeBond * float64(wire.NanoFLEET))
	led := ledger.New(params, *state)
	reg := gateway.NewRegistry(*shadow)
	idv := identity.NewDPoPVerifier(*sso, "", "")
	srv := gateway.NewChainServer(reg, led, idv, *devFaucet)
	srv.EmbedRelay = *embedRelay

	log.Printf("fleet-chain: testnet chain-sim on %s (shadow=%s dev-faucet=%v sso=%s)",
		*listen, *shadow, *devFaucet, *sso)
	log.Printf("fleet-chain: genesis — faucet %s/nullifier, base fees %s/kTok in, %s/kTok out (burned)",
		wire.FmtFLEET(params.FaucetGrant), wire.FmtFLEET(params.BaseFeeInKTok), wire.FmtFLEET(params.BaseFeeOutKTok))
	if err := http.ListenAndServe(*listen, srv.Routes()); err != nil {
		log.Fatal(err)
	}
}

func runGateway(args []string) {
	fs := flag.NewFlagSet("gateway", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:9000", "listen address")
	chain := fs.String("chain", "http://127.0.0.1:9500", "chain endpoint")
	name := fs.String("name", "gateway", "gateway name")
	publicURL := fs.String("public-url", "", "public base URL advertised to peers (default http://<listen>)")
	peers := fs.String("peers", "", "comma-separated bootstrap gateway URLs (peer exchange fills the rest)")
	_ = fs.Parse(args)

	pub := *publicURL
	if pub == "" {
		pub = "http://" + *listen
	}
	var bootstrap []string
	if *peers != "" {
		bootstrap = strings.Split(*peers, ",")
	}
	srv := gateway.NewGatewayServer(*name, *chain, pub, bootstrap)
	log.Printf("fleet-gateway[%s]: PoP on %s (chain %s, public %s, %d bootstrap peers) — relays ciphertext, holds no client keys, sees no plaintext",
		*name, *listen, *chain, pub, len(bootstrap))
	if err := http.ListenAndServe(*listen, srv.Routes()); err != nil {
		log.Fatal(err)
	}
}

func runNode(args []string) {
	fs := flag.NewFlagSet("node", flag.ExitOnError)
	cfgPath := fs.String("config", "", "node config JSON")
	_ = fs.Parse(args)
	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "fleetd node: -config required")
		os.Exit(2)
	}
	cfg, err := node.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	n := node.New(*cfg)
	if err := n.Run(); err != nil {
		log.Fatalf("[%s] %v", cfg.Name, err)
	}
}
