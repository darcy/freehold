module freehold/install

go 1.25.0

require (
	freehold/contract v0.0.0
	freehold/platform v0.0.0
	github.com/spf13/cobra v1.10.2
)

require (
	github.com/btcsuite/btcd/btcec/v2 v2.5.0 // indirect
	github.com/btcsuite/btcd/chainhash/v2 v2.0.0 // indirect
	github.com/decred/dcrd/crypto/blake256 v1.1.0 // indirect
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/pelletier/go-toml/v2 v2.4.3 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
)

replace (
	freehold/contract => ../contract
	freehold/platform => ../platform
)
