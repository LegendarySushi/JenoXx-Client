# JenoXx Client Launcher (.exe)

Eine einzelne Datei: `JenoXxClient.exe` doppelklicken. Kein GitHub, nichts vorinstallieren.

Es oeffnet sich ein eigenes Fenster (Edge/Chrome im App-Modus, lokal auf 127.0.0.1) im Stil des NoRisk Clients.
**Der offizielle Minecraft Launcher wird nicht gebraucht**: JenoXx laedt Minecraft, Fabric, Bibliotheken, Assets
und Java selbst und startet das Spiel direkt.

- **Profile** in der Seitenleiste, jedes mit eigener Minecraft-Version (alle Fabric-faehigen Releases), eigenem Mods-Ordner,
  eigenen Einstellungen und Spielzeit
- **Microsoft-Anmeldung** (Geraetecode): Das Refresh-Token wird mit Windows-DPAPI verschluesselt gespeichert; Passwoerter sieht das Programm nie
- **Mod-Browser**: Mods auf Modrinth suchen und per Klick installieren (inkl. Abhaengigkeiten); installierte Mods ein-/ausschalten oder loeschen
- **Einstellungen** pro Profil: Name, RAM, Java-Pfad, JVM-Argumente; Profil loeschen
- Der JenoXx-Client-Mod (HUD, Zoom, Fullbright) wird nur in Profilen fuer Minecraft 1.21.4 installiert

## Anmeldung

Ein Klick auf "Mit Microsoft anmelden" oeffnet ein eigenes Anmeldefenster (Microsoft-Login, danach geht es automatisch
weiter). Fenster erscheint nicht? Dann gibt es das Ausweichverfahren "Mit Code anmelden" (Code auf microsoft.com/link).

### Einmalig fuer den Entwickler: Azure-App

Microsoft/Mojang verlangen fuer jeden Launcher eine eigene App (Client-ID); die ID des offiziellen Launchers darf nicht
verwendet werden. Das Programm fuehrt durch die Schritte, solange keine ID hinterlegt ist:

1. https://portal.azure.com -> App-Registrierungen -> Neue Registrierung
2. Kontotypen: "Konten in einem beliebigen Organisationsverzeichnis und persoenliche Microsoft-Konten"
3. Authentifizierung -> Plattform "Mobil- und Desktopanwendungen" mit Umleitungs-URI `http://localhost`;
   "Oeffentliche Clientflows zulassen" = Ja
4. App bei Mojang freischalten lassen: https://aka.ms/AppRegInfo (ohne Freigabe antwortet Minecraft mit HTTP 403 "Invalid app registration")
5. Anwendungs-(Client-)ID eintragen

### Damit sich andere Nutzer ohne Einrichtung anmelden koennen

Die Client-ID ist nicht geheim und kann fest im Programm stehen. Auf eine dieser Arten:

- beim Bauen einbetten: `JENOXX_CLIENT_ID=<guid> bash build-exe.sh` (Windows: `set JENOXX_CLIENT_ID=<guid>` und `build-exe.bat`)
- oder eine Datei `client_id.txt` (nur die ID) neben die .exe legen
- oder Umgebungsvariable `JENOXX_CLIENT_ID`

## Absturzhilfe und Reparatur

- Stuerzt Minecraft ab, wertet der Launcher Spielausgabe und Absturzbericht aus und zeigt im Profil eine
  verstaendliche Ursache (zu wenig RAM, falsche Java-Version, inkompatible oder doppelte Mods, Grafikproblem,
  beschaedigte Dateien, abgelaufene Anmeldung) mit passendem Knopf. Verdaechtige Mods aus dem Absturzbericht werden genannt.
- **Zustand pruefen**: kontrolliert Version, Bibliotheken, Java, doppelte Mods, fehlende Fabric API, RAM-Einstellung.
- **Diagnose kopieren**: Textbericht (ohne Anmelde-Token, Benutzername im Pfad durch `~` ersetzt) zum Weitergeben.
- **Reparieren / Neu installieren** prueft jetzt alle Dateien per SHA-1, nicht nur die Groesse, und entpackt die Natives neu.
- Der RAM-Regler richtet sich nach dem Arbeitsspeicher des PCs.

## Mod-Updates und Modpacks

- Mods: **Nach Updates suchen** vergleicht die installierten Mods per Pruefsumme mit Modrinth (passend zu Minecraft-Version
  und Fabric). Einzeln oder mit **Alle aktualisieren** ersetzen; deaktivierte Mods bleiben deaktiviert.
  Der eingebaute JenoXx-Mod wird nicht angefasst.
- **Modpack importieren** (Seitenleiste): Modrinth-Modpacks (`.mrpack`, nur Fabric) suchen oder eine Datei waehlen.
  Es entsteht ein eigenes Profil; Mods werden mit Pruefsumme geladen (nur von cdn.modrinth.com, github.com,
  raw.githubusercontent.com, gitlab.com), die Konfiguration (`overrides`) wird einmalig kopiert und bei einer
  Reparatur nicht ueberschrieben. Ausfuehrbare Dateien (.exe, .bat, .dll ...) im Modpack werden nie geschrieben.

## Profil-Komfort

- **Server beim Start beitreten** (Einstellungen): Ab Minecraft 1.20 per `--quickPlayMultiplayer`, davor per `--server`/`--port`.
- **Spielfenster**: Standard, eigene Groesse oder Vollbild.
- **Profil duplizieren** (Uebersicht): kopiert Einstellungen, Mods, Konfiguration, Resourcepacks, Shaderpacks, Optionen und
  Serverliste - aber keine Welten, Screenshots oder Logs.

## CurseForge

Im Tab "Mods" -> "Mods suchen" kann zwischen **Modrinth** und **CurseForge** gewechselt werden.
CurseForge verlangt einen eigenen, kostenlosen API-Schluessel pro Nutzer (Antrag ueber das Formular im CurseForge-Supportartikel 9000208346, mit Pruefung und Antwort per E-Mail).
Er wird im Launcher einmal eingetragen und mit Windows DPAPI verschluesselt in
`%LOCALAPPDATA%\JenoXxClient\curseforge.key` gespeichert (oder per Umgebungsvariable `JENOXX_CF_KEY`).
Der Schluessel wird bewusst **nicht** in die EXE eingebaut: Die CurseForge-Bedingungen verbieten die Weitergabe.

- Installiert werden die neueste Fabric-Version fuer die Minecraft-Version des Profils samt Pflicht-Abhaengigkeiten.
- Downloads werden per SHA-1 geprueft. Eine Mod, deren Mod-ID schon vorhanden ist (z. B. von Modrinth), wird nicht doppelt abgelegt.
- Verbietet der Autor Downloads ausserhalb von CurseForge, meldet der Launcher das und oeffnet die Mod-Seite zum manuellen Download.
- Modrinth-Abrufe laufen zuerst direkt im Fenster, bei Fehlern ueber den Launcher selbst (hilft bei VPN/Firewall).

## Daten

- Profile, Konfiguration und Konto: `%LOCALAPPDATA%\JenoXxClient\` (`profiles.json`, `config.json`, `account.json`)
- Spieldateien liegen im ueblichen `.minecraft` (gemeinsame `libraries`, `assets`, `versions`, `runtime`);
  jedes Profil hat einen eigenen Ordner `.minecraft\jenoxx-<profil>` fuer Mods, Welten und Einstellungen
- Importierte Modpacks: `%LOCALAPPDATA%\JenoXxClient\packs\`
- Startfehler: `%LOCALAPPDATA%\JenoXxClient\error.log`

## Installation eines Profils

1. Minecraft-Version und Fabric Loader
2. Bibliotheken und Assets (mehrere hundert MB beim ersten Mal)
3. Java-Laufzeit von Mojang
4. Fabric API (Modrinth, sonst Fabric-Maven)
5. JenoXx-Mod (nur 1.21.4): Quellcode steckt in der .exe, wird beim ersten Mal mit JDK 21 + Gradle gebaut
   (laedt ca. 1 GB), spaetere Starts nutzen die fertige Jar. Eine `jenoxx-client-*.jar` neben der .exe wird bevorzugt.

Option: `--no-open` (Fenster nicht oeffnen, URL ausgeben).

## Neu bauen (Entwickler)

`build-exe.sh` bzw. `build-exe.bat` (Go 1.22+): kopiert den Quellcode aus `../jenoxx-client` in `modsrc/` und baut die .exe.

## Logo / Icon

`ui/logo.png` ist das Logo im Fenster, `assets/icon.ico` das EXE-Icon. Nach einem Wechsel des Icons
`python3 tools/make_syso.py` ausfuehren (erzeugt `rsrc_windows_amd64.syso`, wird beim Build automatisch eingebunden).

## Spielerkopf

Im Anmeldefeld zeigt der Launcher den Kopf deines Skins. Er wird direkt von Mojang geladen (sessionserver.mojang.com und textures.minecraft.net), nicht ueber einen Drittdienst, und fuer einige Stunden zwischengespeichert. Ohne eigenen Skin oder ohne Verbindung wird einfach kein Bild gezeigt.

## Sicherheit

- **Lokale Oberflaeche:** Der Launcher lauscht nur auf 127.0.0.1 (nicht im Netzwerk), mit einem zufaelligen 256-Bit-Token im Pfad. Anfragen mit falschem Host (DNS-Rebinding), fremdem Origin oder `Sec-Fetch-Site: cross-site` werden abgelehnt. Die Seite hat eine strenge Content-Security-Policy (kein fremder Code, Verbindungen nur zu Modrinth), `no-referrer`, `nosniff` und `X-Frame-Options: DENY`. Der Server hat Timeouts gegen haengende Verbindungen.
- **Einstellungen:** Java-Pfad nur auf eine lokale `java.exe`/`javaw.exe` (keine Netzwerkpfade). JVM-Argumente nur Speicher-/GC-/`-D`-Optionen; `-javaagent`, `-agentpath`, Klassenpfad-Eingriffe, `-XX:OnError`, `fabric.addMods` u. Ae. sind gesperrt. Die Pruefung gilt auch beim Spielstart.
- **Downloads:** Weiterleitungen nur per HTTPS und max. 5; beim Wechsel des Servers werden geheime Header (z. B. der CurseForge-Schluessel) entfernt. Antworten im Speicher sind auf 300 MB begrenzt. Dateien werden per SHA-1 geprueft, Archive gegen Pfad-Ausbrueche (zip slip) geschuetzt, Modpack-Dateien ohne ausfuehrbare Endungen.
- **Anmeldedaten:** Refresh-Token und CurseForge-Schluessel liegen mit Windows-DPAPI verschluesselt und nur fuer deinen Benutzer lesbar im Profilordner. Das Spiel-Protokoll blendet das Zugriffstoken aus.

Grenzen: Wer bereits Programme unter deinem Windows-Benutzer ausfuehren kann (z. B. Schadsoftware), kann auch diese Daten lesen - dagegen hilft nur ein sauberes System. Nur Mods und Modpacks aus vertrauenswuerdigen Quellen installieren: Eine Mod ist ausfuehrbarer Code.

## Animationen

Die Oberflaeche ist animiert: Ansichten und Karten blenden nacheinander ein, die Tab-Linie gleitet, Dialoge poppen auf (mit unscharfem Hintergrund), Hinweise gleiten rein und raus, Buttons haben Hover-Effekt und Klick-Welle, der Spielen-Button leuchtet und glaenzt, der Fortschrittsbalken schimmert, erledigte Schritte "poppen", bei einem Absturz wackelt die Absturzkarte kurz. Wer in Windows "Animationseffekte" ausschaltet (Barrierefreiheit), bekommt die Oberflaeche ohne Bewegung.

## Spielen ohne Anmeldung im JenoXx-Launcher

Der Button **Im offiziellen Launcher spielen** (Uebersicht, und im Dialog "Azure-App einrichten") traegt das installierte Profil in die `launcher_profiles.json` des offiziellen Minecraft Launchers ein (Name "JenoXx - <Profil>", eigener Spielordner, deine RAM-Einstellung) und oeffnet ihn. Dort meldest du dich wie gewohnt bei Mojang an und startest das Profil. Bestehende Profile und Einstellungen bleiben erhalten, einmalig wird `launcher_profiles.json.jenoxx-backup` angelegt. Ist das Profil noch nicht installiert, wird zuerst installiert. Server-Direktbeitritt und Fenstergroesse aus den JenoXx-Einstellungen gelten nur beim Start ueber JenoXx selbst.
