#!/usr/bin/env bash
# Baut JenoXxClient.exe (Windows). Quellcode des Mods wird eingebettet;
# die Jar wird beim ersten Start automatisch auf dem PC des Nutzers gebaut.
set -euo pipefail
cd "$(dirname "$0")"
rm -rf modsrc && mkdir modsrc && touch modsrc/.keep
cp ../jenoxx-client/build.gradle ../jenoxx-client/gradle.properties ../jenoxx-client/settings.gradle modsrc/
cp -r ../jenoxx-client/src modsrc/src
rm -f payload/*.jar
# Optional: Azure-Client-ID einbetten, dann melden sich Nutzer ohne Einrichtung an:  JENOXX_CLIENT_ID=<guid> bash build-exe.sh
if [ -n "${JENOXX_CLIENT_ID:-}" ]; then printf "%s\n" "$JENOXX_CLIENT_ID" > client_id.txt; else printf "\n" > client_id.txt; fi
GOOS=windows GOARCH=amd64 go build -ldflags="-s -w -H=windowsgui" -o JenoXxClient.exe .
echo "Fertig: JenoXxClient.exe"
