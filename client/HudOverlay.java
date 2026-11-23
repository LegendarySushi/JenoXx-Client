package dev.jenoxx.client;

import net.minecraft.client.DeltaTracker;
import net.minecraft.client.Minecraft;
import net.minecraft.client.gui.GuiGraphics;
import net.minecraft.client.player.LocalPlayer;

import java.util.ArrayList;
import java.util.List;
import java.util.Locale;

/** Einfaches Text-HUD oben links. */
public final class HudOverlay {
	private static final int COLOR = 0xFFFFFFFF;
	private static final int PADDING = 4;
	private static final int LINE_HEIGHT = 10;

	private static long windowStart = System.nanoTime();
	private static int frames = 0;
	private static int fps = 0;

	private HudOverlay() {}

	/** Eigener FPS-Zaehler (zaehlt gerenderte HUD-Frames pro Sekunde). */
	private static void countFrame() {
		frames++;
		long now = System.nanoTime();
		if (now - windowStart >= 1_000_000_000L) {
			fps = frames;
			frames = 0;
			windowStart = now;
		}
	}

	public static void render(GuiGraphics graphics, DeltaTracker tickCounter) {
		countFrame();
		Minecraft mc = Minecraft.getInstance();
		if (!Settings.hudEnabled || mc.options.hideGui || mc.player == null) {
			return;
		}

		LocalPlayer p = mc.player;
		List<String> lines = new ArrayList<>();
		lines.add("JenoXx Client");
		lines.add(fps + " FPS");
		lines.add(String.format(Locale.ROOT, "XYZ: %.1f / %.1f / %.1f", p.getX(), p.getY(), p.getZ()));
		lines.add("Blick: " + p.getDirection().getName());
		if (Settings.fullbright) {
			lines.add("Fullbright: an");
		}

		int y = PADDING;
		for (String line : lines) {
			graphics.drawString(mc.font, line, PADDING, y, COLOR, true);
			y += LINE_HEIGHT;
		}
	}
}
