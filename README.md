# Toon MHI airco-plugins

Losse connectors voor de native MHI Airco-app op Toon.

## Beschikbare plugins

- `wfrac`: Mitsubishi Heavy Industries WF-RAC via de lokale netwerk-API.
- `esphome`: MHI ESPHome via de versleutelde native ESPHome-API.

De Toon-app haalt `catalog.json` op, controleert platform, API-versie en SHA-256,
en test de connector voordat een bestaande versie wordt vervangen. De centrale
app blijft verantwoordelijk voor thermostaatblokkering en bivalente regeling.

## Bouwen

```sh
./build.sh 1.0.0
```

Dit maakt statische Linux ARMv7-binaries in `dist/`. Er horen geen Toon-config,
airco-adressen, inloggegevens of ESPHome-sleutels in deze repository.

## Uitgave

1. Bouw de twee binaries.
2. Publiceer ze als assets van een GitHub-release.
3. Pas versie, URL en SHA-256 in `catalog.json` aan.

De connector-API-versie is momenteel `1`.
