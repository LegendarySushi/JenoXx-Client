package dev.jenoxx.client;

import net.minecraft.client.Minecraft;
import net.minecraft.world.effect.MobEffectInstance;
import net.minecraft.world.effect.MobEffects;

/** Client-seitige Funktionen: Zoom und Fullbright. */
public final class Features {
	private static final int NIGHT_VISION_TICKS = 400;
	/** Unter ~200 Ticks beginnt Nachtsicht zu flackern, daher rechtzeitig erneuern. */
	private static final int REFRESH_THRESHOLD = 240;

	private static boolean zooming = false;
	private static int savedFov = 70;
	private static boolean nightVisionApplied = false;

	private Features() {}

	/** Zoom: FOV absenken, solange die Taste gehalten wird, danach alten Wert wiederherstellen. */
	public static void updateZoom(Minecraft mc, boolean keyDown) {
		var fov = mc.options.fov();
		if (keyDown && !zooming) {
			savedFov = fov.get();
			zooming = true;
			fov.set(Settings.ZOOM_FOV);
		} else if (!keyDown && zooming) {
			zooming = false;
			fov.set(savedFov);
		}
	}

	/** Fullbright ueber einen rein clientseitigen Nachtsicht-Effekt (ohne Icon/Partikel). */
	public static void updateFullbright(Minecraft mc, boolean enabled) {
		if (mc.player == null) {
			nightVisionApplied = false;
			return;
		}

		if (enabled) {
			MobEffectInstance current = mc.player.getEffect(MobEffects.NIGHT_VISION);
			if (current == null || current.getDuration() < REFRESH_THRESHOLD) {
				mc.player.addEffect(new MobEffectInstance(
						MobEffects.NIGHT_VISION, NIGHT_VISION_TICKS, 0, false, false, false));
			}
			nightVisionApplied = true;
		} else if (nightVisionApplied) {
			mc.player.removeEffect(MobEffects.NIGHT_VISION);
			nightVisionApplied = false;
		}
	}
}
