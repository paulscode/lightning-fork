# The bridge's SHA256 node

A bridge pays invoices on the SHA256 chain through a Lightning node there: a
stock lnd, never this fork, because every change Lightning Fork makes is one
that must not apply to that chain. There are two ways to have one.

- **Lightning Fork runs it** (`bridgerpc.sha256.supervised`). The platform
  package starts a stock lnd for you; Lightning Fork gives it its seed, creates
  its wallet and keeps its credentials. Nothing new to write down. This is the
  default in the StartOS and Umbrel packages.
- **You already run one.** Point `bridgerpc.sha256.rpchost`,
  `bridgerpc.sha256.tlscertpath` and `bridgerpc.sha256.macaroonpath` at it, as
  [bridge.md](bridge.md) describes.

This document is about the first: how it works, what to back up, and how to
recover that node with or without Lightning Fork.

## What it costs

A Lightning node, not a chain node: it reads the full node on the SHA256
chain you already run, over RPC, and keeps its own wallet, channels and gossip graph,
about 1 to 3 GB. It starts empty. The bridge cannot pay anyone until you send
it coins on the SHA256 chain and open a channel from it, which the packages
help you do.

## How the pieces fit

| Who | Does |
| --- | --- |
| The platform | Runs the stock lnd, from the official image, once the bridge is on. Starts it with `--wallet-unlock-password-file` and `--wallet-unlock-allow-create`, so it waits for its wallet to be created and unlocks itself after that. Restarts it if it stops. |
| Lightning Fork | Writes that password (random, kept beside the bridge's journal). Creates the node's wallet, once, from a seed derived from its own. Bakes a macaroon with only the 14 calls the bridge makes, and uses that, never the admin one. Refuses the node unless its identity is the one the derived seed gives. |

Every step Lightning Fork takes is repeated before each connection attempt and
does nothing when already done, so a restart of either node at any moment
resumes rather than needing anything undone. `lncli bridge status` reports
where the node is (`sha256_node.state`: `starting`, `creating_wallet`,
`syncing`, `ready`, `locked`, `unreachable`, `not_ours`, `error`) and, in
`detail`, what to do about it.

### Where things are

Under this node's lnd directory (`/root/.lnd` in the packages):

| Path | What |
| --- | --- |
| `sha256-node/` | The SHA256 node's own lnd directory: its wallet, channels, `tls.cert`, and `data/chain/bitcoin/mainnet/admin.macaroon` for your own tools. |
| `sha256-node/data/chain/bitcoin/mainnet/channel.backup` | That node's static channel backup. |
| `data/chain/bitcoin/mainnet/bridge/sha256/wallet.password` | Its wallet password. |
| `data/chain/bitcoin/mainnet/bridge/sha256/bridge.macaroon` | The narrow macaroon the bridge uses. |

Both nodes live in one data directory, so a platform backup of Lightning
Fork carries both. For the SHA256 node it carries the channel backup and not
the wallet or channel database: a channel database from the past can
broadcast an old channel state, which can lose that channel's funds. On a
restore Lightning Fork recreates the node's wallet from the derived seed,
so its on-chain coins are found again, and once the node is known to follow
the SHA256 chain it restores the channels from that backup: their peers
close them and the funds return to the wallet. The backup is first copied to
`channel.backup.restored` beside the wallet password, because the node
rewrites its own file as soon as it starts; the restore is retried every
minute until it works (a peer may be offline), `lncli bridge status` says so
meanwhile, and `channel.backup.restored.done` marks it finished.

A platform backup is only as recent as the last time it ran. Keep a current
copy of `sha256-node/.../channel.backup` as you would this node's own:
channels opened after the last backup come back only from a newer one.

The node listens on 9739 (peers), 10019 (gRPC) and 8089 (REST), clear of
Lightning Fork's own ports.

## The seed

The SHA256 node has a seed of its own. It is **derived** from this node's
wallet, not shared with it and not generated and stored.

Not shared, because a seed from before the BLAKE2b fork controls coins that
exist on both chains. A stock lnd signs with plain `SIGHASH_ALL`, so its
spends of those coins replay onto the BLAKE2b chain. Sharing a seed would also
give both nodes one identity and one set of addresses, and let each one's
channel backups decrypt in the other. The bridge refuses any SHA256 node with
this node's identity, whichever way you run it.

Not stored, because one phrase to keep is the point. Your Lightning Fork
recovery phrase is enough to recreate the SHA256 node exactly.

### The derivation, precisely

1. From the Lightning Fork wallet, take the private key at
   `m/1017'/coin'/1000'/0/0`: key family 1000, index 0, in lnd's scheme.
   `coin` is 0 on mainnet and 1 on the test networks.
2. HKDF-SHA256 ([RFC 5869](https://www.rfc-editor.org/rfc/rfc5869)) with:
   - secret: that key's 32 bytes, big-endian, including any leading zeros;
   - salt: none (HashLen zero bytes);
   - info: the ASCII string `lightning-fork/bridge/sha256-node/seed/v1`;
   - output: 16 bytes.
3. Those 16 bytes are the SHA256 node's aezeed entropy, with the wallet
   birthday 2026-10-01. No wallet derived this way can have a transaction
   before that date, so the rescan starts there.

The constants live in `lnrpc/bridgerpc/seed.go` and are versioned in the info
string; a future derivation would be `v2` beside this one, never a change to
this one.

**One thing that is easy to get wrong.** lnd's wallet (btcwallet) does not
derive keys by textbook BIP32. An old btcutil serialised a private key without
its leading zero bytes when deriving a hardened child, and btcwallet keeps that
behaviour for compatibility, but only where the parent key was never stored
and read back (keys read back are 32 bytes again):

| Level | Derived from | How |
| --- | --- | --- |
| `1017'` | the master key | either; the master is always 32 bytes |
| `coin'` | the purpose key | **non-standard** (`DeriveNonStandard`) |
| `1000'` (and any family above 0) | the coin type key | **standard** BIP32 |
| `0`, `0` | | not hardened, so the private key is not hashed; either |

The two methods differ only when the parent private key starts with a zero
byte, about one time in 256 per level, which is exactly why a tool that uses
plain BIP32 would agree with lnd almost always and be wrong now and then.
`LndKeyAt` in `seed.go` implements this, and `seed_test.go` measures it
against a real btcwallet, freshly created and reopened, on seeds chosen so
that each level makes a difference.

## Recovering the SHA256 node

### With Lightning Fork

Restore Lightning Fork from its phrase as usual and turn the bridge on in the
same mode. Lightning Fork derives the same seed and creates the same node,
asking lnd to recover its on-chain funds. If the node's `channel.backup` is
already in place (as after a platform restore), its channels are restored
with the wallet; otherwise restore them from your copy, for example with
`lncli restorechanbackup`.

If only the SHA256 node's directory survived and Lightning Fork's did not,
Lightning Fork finds a wallet whose password it no longer has, and says so
rather than overwriting anything: move `sha256-node/` aside, keeping its
`channel.backup`, and the node is created again from the same seed.

### Without Lightning Fork

Ask Lightning Fork for the seed while you still can:

```
lncli bridge sha256seed
```

or, on StartOS, the **SHA256 Node Recovery Phrase** action. Either gives:

- `mnemonic`: 24 aezeed words, no passphrase. Each call salts them afresh,
  so the words change every time; every version restores the same node.
- `extended_master_key`: the same seed as a BIP32 root key.
- `identity_pubkey`: the identity the restored node must report. Check it.

Then, on any stock lnd for the SHA256 chain: `lncli create`, choose to restore
from a seed (the words) or from an extended master key, and give it the
node's `channel.backup` to recover its channels. Channels come back the way
every lnd channel backup works: the peers close them and the funds return to
the wallet.

This was tested end to end: `scripts/scenario-supervised.sh` in the lab
restores a supervised node in a separate stock lnd from the exported words and
its channel backup, checks the identity, and gets its channel funds back.

### By hand

If Lightning Fork itself cannot be run, derive the seed from its recovery
phrase with any BIP32 and HKDF implementation, following the derivation above
(mind the table), and restore with the 16-byte entropy or the BIP32 root key
built from it.

## Which chain it follows

A stock lnd checks neither proof of work nor which side of the fork its chain
backend took, and the two chains share their genesis block, so pointed at a
node that follows BLAKE2b it would sync, give out addresses and open channels
on the wrong chain. Before using the SHA256 node, the bridge reads its block
at the BLAKE2b activation height: it must not be this node's block there, and
its header must be an 80-byte header that hashes to the id the node reports.
On mainnet a node that cannot answer (one built without `chainrpc`, or not yet
past that height) is waited for or refused; the official lnd images have
`chainrpc`.

## When the node is down

A swap in progress is safe across a restart of either node. If the SHA256
node does not answer when the bridge goes to pay, nothing is sent, and the
swap waits and is paid when the node is back, provided the payer's HTLC still
leaves enough time; otherwise the payer is refunded. A payment that did reach
the node is never sent again.
