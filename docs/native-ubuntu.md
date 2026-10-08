# Running Lightning Fork on Ubuntu, next to your own Knots node

This guide is for running Lightning Fork straight on a Linux machine, built
from source, next to a Bitcoin Knots node you already run there. If you use
StartOS or Umbrel, install Lightning Fork from their app stores instead.

It was tested on Ubuntu 22.04 and 24.04 (Linux Mint and Debian work the same
way), on x86_64; arm64 machines such as a Raspberry Pi 4 or 5 are supported
too. You need about 2 GB of free RAM for the build and a few GB of disk.

`scripts/native-ubuntu.sh` does everything below for you and prints each
command before it runs it. You can also read it, or follow this page and type
the commands yourself.

## 1. Check your Knots node

Lightning Fork follows the Bitcoin BLAKE2b chain, which split from the SHA256
chain at block 961640. **Knots before v29.4.1 follows the SHA256 chain**, so
check your version first:

```sh
bitcoin-cli -version          # needs v29.4.1 or later
bitcoin-cli getblockhash 961640
# 0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb
```

If the hash is different, your node is on the SHA256 chain: upgrade Knots
(https://bitcoinknots.org) before going on. If `getblockhash` says the block
is out of range, Knots is still syncing; Lightning Fork waits for it.

Lightning Fork needs these in Knots' `bitcoin.conf` (usually
`~/.bitcoin/bitcoin.conf`). Add what is missing and restart Knots:

```ini
server=1
zmqpubrawblock=tcp://127.0.0.1:28332
zmqpubrawtx=tcp://127.0.0.1:28333
```

A pruned node is fine, and `txindex` is not needed.

## 2. Get the code and build it

```sh
git clone https://github.com/paulscode/lightning-fork.git
cd lightning-fork
scripts/native-ubuntu.sh check     # looks around, changes nothing
scripts/native-ubuntu.sh all       # packages, Go, build, configure
```

`all` runs four steps, which you can also run one at a time:

| Step | What it does |
| --- | --- |
| `deps` | `sudo apt-get install git make curl ca-certificates` (only what is missing) |
| `go` | Downloads Go 1.26.8 from go.dev into `~/.local/go`, after checking the file against Go's published SHA-256. Skipped if you already have a new enough Go. |
| `build` | `make release-install`: builds `lnd` and `lncli` into `~/go/bin` (about 2 minutes on a recent machine, longer on a Pi) |
| `configure` | Checks Knots (version, chain, RPC login, ZMQ) and writes `~/.lnd/lnd.conf`. It never overwrites an existing `lnd.conf`. |

The default branch, `blake2b`, is the latest release plus documentation. To
find `go`, `lnd` and `lncli` in new terminals, add this line to `~/.bashrc`
(the script prints it with your paths):

```sh
export PATH="$HOME/.local/go/bin:$HOME/go/bin:$PATH"
```

### If Knots runs as another user, or keeps its data elsewhere

`configure` reads Knots' `.cookie` file to log in. Tell it where things are:

```sh
BITCOIN_CLI="bitcoin-cli -datadir=/data/bitcoin" BITCOIN_DIR=/data/bitcoin \
  scripts/native-ubuntu.sh configure
```

If Knots runs as a different user (for example a `bitcoin` system user), the
cookie usually isn't readable by you. Either add yourself to that user's group
(and set `rpccookieperms=group` in `bitcoin.conf`), or give Lightning Fork its
own RPC login:

```sh
python3 share/rpcauth/rpcauth.py lightningfork   # from the Knots download
# put the rpcauth=... line it prints in bitcoin.conf, restart Knots, then:
BITCOIND_RPCUSER=lightningfork BITCOIND_RPCPASS='<the password it printed>' \
  scripts/native-ubuntu.sh configure
```

## 3. Start it and create the wallet

```sh
lnd                 # leave this running in its own terminal
```

In a second terminal:

```sh
lncli create
```

Choose a wallet password, answer `n` to create a new seed, and write the 24
words down on paper. **The seed and the file `channel.backup` are what you
need to recover your funds**; keep a copy of
`~/.lnd/data/chain/bitcoin/mainnet/channel.backup` somewhere off this machine
too, and refresh it after opening or closing channels.

Then check it is following the chain:

```sh
lncli getinfo       # synced_to_chain: true, and a version ending in -blake2b.N
cat ~/.lnd/data/chain/bitcoin/mainnet/chain-identity.json   # "state": "confirmed"
```

Every time `lnd` starts, the wallet is locked until you run `lncli unlock`.

### Running it in the background

```sh
scripts/native-ubuntu.sh service
sudo loginctl enable-linger $USER    # keep it running after you log out and across reboots
journalctl --user -u lightning-fork -f
```

You still need `lncli unlock` after each start. lnd can unlock itself from a
password file (`wallet-unlock-password-file=` in `lnd.conf`, see
`sample-lnd.conf`), but then anyone who can read that file can spend your
funds; only do that on a machine you trust completely.

## 4. Tor

Most nodes on this network can only be reached as `.onion` addresses, so you
will want Tor:

```sh
sudo apt install tor
scripts/native-ubuntu.sh tor         # adds a [Tor] section to lnd.conf
```

That lets your node reach onion peers. For an onion address of your own (so
others can connect to you without any router setup), also put these in
`/etc/tor/torrc`:

```
ControlPort 9051
CookieAuthentication 1
CookieAuthFileGroupReadable 1
```

then `sudo usermod -aG debian-tor $USER`, log out and in again,
`sudo systemctl restart tor`, and add `tor.v3=true` and
`tor.control=127.0.0.1:9051` to the `[Tor]` section of `lnd.conf`.
(If you run the `tor` step after setting this up, it adds them for you.)
Restart lnd after changing `lnd.conf`.

## 5. First peers and channels

Lightning Fork has no DNS seeds, so a new node knows nobody and logs
`Unable to retrieve initial bootstrap peers: no addresses found` until you
give it one peer. One is enough: from that peer it learns the network and
connects to more nodes by itself.

```sh
lncli connect <node public key>@<address>:<port>
lncli getnetworkinfo        # num_nodes and num_channels grow within a minute
```

To open a channel, fund the wallet and open one to a well-connected node:

```sh
lncli newaddress p2tr       # send BTCB2 to this address
lncli walletbalance         # wait for the confirmation
lncli openchannel --node_key=<public key> --local_amt=<sats>
lncli pendingchannels       # opens after a few confirmations
```

## Being reachable over clearnet

Without Tor's onion address, others can reach you only if port 9735 is
forwarded to this machine on your router. Then tell lnd your public address
by adding `externalip=<your public IP>:9735` under `[Application Options]`.
You can open channels to others either way.

## Updating

```sh
cd lightning-fork
git pull
scripts/native-ubuntu.sh build
# then restart lnd (Ctrl-C and start it again, or:
systemctl --user restart lightning-fork)
```

## When something goes wrong

| You see | It means |
| --- | --- |
| `Lightning Fork needs Bitcoin Knots v29.4.1 or later` | Your Knots follows the SHA256 chain. Upgrade it. |
| `block 961640 is ...` with another hash | Same: the node is on the SHA256 chain. |
| `can't read .../.cookie` | Knots isn't running, `BITCOIN_DIR` points elsewhere, or the cookie belongs to another user (section 2). |
| `Knots does not publish blocks and transactions over ZMQ` | Add the two `zmqpub...` lines to `bitcoin.conf` and restart Knots. |
| `port 9735 is in use` | Another Lightning node runs here. Change `listen`, `rpclisten` and `restlisten` in `lnd.conf`, and use `lncli --rpcserver=127.0.0.1:<rpc port>`. |
| `chain-identity.json` says `refused` | lnd checked your node and it is not on the BLAKE2b chain; the `reason` field says why. |
| `no addresses found` in the log | Normal until you connect to a first peer (section 5). |
| `wallet locked` errors | Run `lncli unlock`. |

Everything else is standard `lnd`: see `sample-lnd.conf` for every option,
and [blake2b.md](blake2b.md) for what Lightning Fork changes.
