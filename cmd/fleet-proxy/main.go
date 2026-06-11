// fleet-proxy is the Fleet local proxy (whitepaper §11.1, recommended
// client mode): a single binary on localhost speaking plain OpenAI HTTP.
// Point any OpenAI client at it (base_url) and it handles node selection,
// end-to-end HPKE encryption, voucher payment, and receipt verification.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"alien.org/fleet/internal/proxy"
	"alien.org/fleet/internal/wire"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	gw := flag.String("gateway", "http://127.0.0.1:9000", "Fleet gateway URL")
	listen := flag.String("listen", "127.0.0.1:8080", "OpenAI API listen address")
	model := flag.String("default-model", "gemma4", "default model alias")
	stateDir := flag.String("state-dir", defaultStateDir(), "proxy state directory")
	deposit := flag.Uint64("deposit", 0, "channel deposit in whole FLEET (0 = 90% of balance)")
	agentIDCLI := flag.String("agent-id-cli", "", "path to agent-id-auth cli.mjs for identity-gated faucet")
	flag.Parse()

	e, err := proxy.NewEngine(proxy.Config{
		Gateway: *gw, Listen: *listen, DefaultModel: *model,
		StateDir: *stateDir, DepositNano: *deposit * wire.NanoFLEET,
		AgentIDCLI: *agentIDCLI,
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fleet-proxy: session key %s…", e.SessionPubHex()[:16])
	e.DiscoverGateways() // bootstrap entrypoint -> peer exchange -> latency race (§11.1)
	go func() {
		for {
			time.Sleep(2 * time.Minute)
			e.DiscoverGateways()
		}
	}()
	if err := e.EnsureFunded(); err != nil {
		log.Fatalf("funding: %v", err)
	}
	srv := &proxy.Server{E: e}
	log.Printf("fleet-proxy: OpenAI-compatible API on http://%s/v1 (default model %q, gateway %s)",
		*listen, *model, *gw)
	if err := http.ListenAndServe(*listen, srv.Routes()); err != nil {
		log.Fatal(err)
	}
}

func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".fleet-proxy"
	}
	return filepath.Join(home, ".fleet-proxy")
}
