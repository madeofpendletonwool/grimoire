#!/usr/bin/env python3
"""Refresh internal/encounter/testdata/srd_corpus.json.

The corpus is a trimmed snapshot of the Open5e SRD bestiary — the same
records the encounter catalog mirrors — so the statblock harness can parse
every action and run the DMG CR calculator over every creature offline, with
no API on the test path. It is regenerated on purpose, never by a test at
runtime:

    python3 internal/encounter/testdata/fetch_corpus.py

The selection mirrors Catalog.fetchAll exactly: page through /v2/creatures/
scoped to the SRD documents (most preferred first, srd-2024 > srd > srd-2014),
keep the best-ranked document's version of each squashed name, and drop
records with no challenge rating. The SRD is CC-BY-4.0; this file carries
the same licence as the corpus it mirrors, with Wizards of the Coast
attribution in docs/development/statblock.md.
"""

import json
import sys
import urllib.request
import urllib.parse
from pathlib import Path

BASE = "https://api.open5e.com/v2/creatures/"
DOCS = ["srd-2024", "srd", "srd-2014"]  # most preferred first, as srdDocKeys
OUT = Path(__file__).with_name("srd_corpus.json")


def squash(name):
    return "".join(c for c in name.lower() if c.isalnum())


def get(url):
    req = urllib.request.Request(
        url, headers={"user-agent": "grimoire-corpus-fetch/1.0 (+https://github.com/madeofpendletonwool/grimoire)",
                      "accept": "application/json"})
    with urllib.request.urlopen(req, timeout=60) as resp:
        return json.load(resp)


def trim(rec):
    """Keep exactly the fields creatureRecord's JSON tags read."""
    return {
        "key": rec.get("key", ""),
        "name": rec.get("name", ""),
        "document": {"key": rec.get("document", {}).get("key", "")},
        "type": {"name": rec.get("type", {}).get("name", "")} if rec.get("type") else None,
        "size": {"name": rec.get("size", {}).get("name", "")} if rec.get("size") else None,
        "challenge_rating": rec.get("challenge_rating"),
        "alignment": rec.get("alignment", ""),
        "armor_class": rec.get("armor_class", 0),
        "hit_points": rec.get("hit_points", 0),
        "hit_dice": rec.get("hit_dice", ""),
        "speed_all": rec.get("speed_all") or {},
        "ability_scores": rec.get("ability_scores") or {},
        "saving_throws_all": rec.get("saving_throws_all") or {},
        "skill_bonuses": rec.get("skill_bonuses") or {},
        "proficiency_bonus": rec.get("proficiency_bonus"),
        "languages": {"as_string": rec.get("languages", {}).get("as_string", "")},
        "passive_perception": rec.get("passive_perception", 0),
        "darkvision_range": rec.get("darkvision_range"),
        "blindsight_range": rec.get("blindsight_range"),
        "tremorsense_range": rec.get("tremorsense_range"),
        "truesight_range": rec.get("truesight_range"),
        "resistances_and_immunities": {
            "damage_immunities_display": rec.get("resistances_and_immunities", {}).get("damage_immunities_display", ""),
            "damage_resistances_display": rec.get("resistances_and_immunities", {}).get("damage_resistances_display", ""),
            "damage_vulnerabilities_display": rec.get("resistances_and_immunities", {}).get("damage_vulnerabilities_display", ""),
            "condition_immunities_display": rec.get("resistances_and_immunities", {}).get("condition_immunities_display", ""),
        },
        "actions": [
            {
                "name": a.get("name", ""),
                "desc": a.get("desc", ""),
                "action_type": a.get("action_type", ""),
                "usage_limits": a.get("usage_limits"),
                "legendary_action_cost": a.get("legendary_action_cost") or 0,
            }
            for a in rec.get("actions", [])
        ],
        "traits": [
            {"name": t.get("name", ""), "desc": t.get("desc", "")}
            for t in rec.get("traits", [])
        ],
    }


def main():
    best = {}
    url = BASE + "?" + urllib.parse.urlencode({
        "limit": 100,
        "ordering": "name",
        "document__key__in": ",".join(DOCS),
    })
    pages = 0
    while url and pages < 40:
        page = get(url)
        pages += 1
        for rec in page.get("results", []):
            key = squash(rec.get("name", ""))
            if not key:
                continue
            doc = rec.get("document", {}).get("key", "")
            if doc not in DOCS:
                continue  # community document, not SRD
            rank = DOCS.index(doc)
            prev = best.get(key)
            if prev and prev[0] <= rank:
                continue
            best[key] = (rank, rec)
        url = page.get("next", "")

    out = []
    for _rank, rec in best.values():
        if rec.get("challenge_rating") is None:
            continue  # a creature with no challenge rating cannot be budgeted
        out.append(trim(rec))
    out.sort(key=lambda r: r["name"])

    payload = {
        "_comment": "Snapshot of the Open5e SRD bestiary for the offline "
                    "statblock harness. Regenerate with fetch_corpus.py; see "
                    "docs/development/statblock.md.",
        "source": BASE,
        "documents": DOCS,
        "count": len(out),
        "creatures": out,
    }
    OUT.write_text(json.dumps(payload, ensure_ascii=False, indent=1) + "\n", encoding="utf-8")
    print(f"wrote {len(out)} creatures to {OUT} ({OUT.stat().st_size // 1024} KiB, {pages} pages)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
