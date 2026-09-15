#!/usr/bin/env python3
"""What is on the Bitcoin BLAKE2b Lightning network, from one node's view.

    lncli getinfo      > /tmp/info.json
    lncli describegraph > /tmp/graph.json
    python3 contrib/network-census.py /tmp/graph.json /tmp/info.json

Or in one line, if lncli is on the path:

    python3 contrib/network-census.py <(lncli describegraph) <(lncli getinfo)

This exists because a claim about how much is at stake should be a number.
Channels here are announced under this chain's chain_hash, so they are exactly
what a change of chain identity would strand, and "some, and growing" is not
something anyone can weigh against a migration plan.

A caveat the output repeats, because it matters: this is one node's gossip
view. It sees public channels that have been announced and that this node has
learned about. Private channels are invisible to it, and a node that has been
running longer or is better connected will see more. Treat it as a floor.
"""

import json
import sys
from collections import Counter
from datetime import datetime, timezone


def load(path):
    with open(path) as f:
        return json.load(f)


def ago(ts, now):
    if not ts:
        return None
    return (now - ts) / 86400.0


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)

    graph = load(sys.argv[1])
    info = load(sys.argv[2]) if len(sys.argv) > 2 else {}

    now = datetime.now(timezone.utc).timestamp()
    nodes = graph.get("nodes", [])
    edges = graph.get("edges", [])

    capacity = sum(int(e.get("capacity", 0)) for e in edges)

    # A channel counts as live if either side has published a policy update
    # recently. An announced channel whose policies have gone quiet for weeks
    # is usually a node that has gone away rather than a channel in use.
    fresh = Counter()
    for e in edges:
        updates = [
            e.get("last_update", 0),
            (e.get("node1_policy") or {}).get("last_update", 0),
            (e.get("node2_policy") or {}).get("last_update", 0),
        ]
        days = ago(max(updates), now)
        if days is None:
            fresh["unknown"] += 1
        elif days <= 1:
            fresh["1d"] += 1
        elif days <= 7:
            fresh["7d"] += 1
        elif days <= 30:
            fresh["30d"] += 1
        else:
            fresh["older"] += 1

    # How concentrated is it: if one node is in most channels, the network is
    # one hub rather than a network, and that is worth knowing before citing
    # a channel count as evidence of adoption.
    degree = Counter()
    for e in edges:
        degree[e.get("node1_pub")] += 1
        degree[e.get("node2_pub")] += 1

    print("Bitcoin BLAKE2b Lightning, as one node sees it")
    print("=" * 46)
    if info:
        print("Reporting node : %s" % info.get("alias", "?"))
        print("Version        : %s" % info.get("version", "?"))
        print("Block height   : %s" % info.get("block_height", "?"))
        print("Its own peers  : %s" % info.get("num_peers", "?"))
        print("Its own chans  : %s active, %s pending" % (
            info.get("num_active_channels", "?"),
            info.get("num_pending_channels", "?")))
        print()

    print("Nodes in graph  : %d" % len(nodes))
    print("Channels        : %d" % len(edges))
    print("Total capacity  : %d sat (%.4f coins)" % (capacity, capacity / 1e8))
    if edges:
        print("Median channel  : %d sat" % sorted(
            int(e.get("capacity", 0)) for e in edges)[len(edges) // 2])
    print()

    print("Last policy update per channel")
    for label, key in (("within 1 day", "1d"), ("within 7 days", "7d"),
                       ("within 30 days", "30d"), ("older", "older"),
                       ("unknown", "unknown")):
        if fresh[key]:
            print("  %-15s %d" % (label, fresh[key]))
    print()

    if degree:
        top = degree.most_common(5)
        busiest = top[0][1]
        print("Most connected nodes (channel count)")
        for pub, n in top:
            alias = next((x.get("alias") for x in nodes
                          if x.get("pub_key") == pub), "")
            print("  %-16s %-20s %d" % (pub[:16], alias[:20], n))
        if edges:
            print()
            print("  Busiest node is in %.0f%% of channels."
                  % (100.0 * busiest / len(edges)))
    print()

    print("What this is and is not")
    print("  This is one node's gossip view: public channels it has been")
    print("  told about. Private channels are invisible to it, and a")
    print("  better connected node will see more. It is a floor, not a")
    print("  census.")
    print()
    print("  Every channel counted here is announced under this chain's")
    print("  chain_hash. They are what a change of chain identity would")
    print("  strand, which is why the number is worth having before")
    print("  anyone argues about the timing of one.")


if __name__ == "__main__":
    main()
