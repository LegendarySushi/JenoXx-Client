package dev.jenoxx.client;

/** Laufzeit-Einstellungen (werden nicht gespeichert). */
public final class Settings {
	public static boolean hudEnabled = true;
	public static boolean fullbright = false;

	/** FOV, der beim Zoomen gesetzt wird (Vanilla-Minimum ist 30). */
	public static final int ZOOM_FOV = 30;

	private Settings() {}
}
