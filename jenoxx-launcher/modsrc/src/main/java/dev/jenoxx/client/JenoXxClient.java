package dev.jenoxx.client;

import com.mojang.blaze3d.platform.InputConstants;
import net.fabricmc.api.ClientModInitializer;
import net.fabricmc.fabric.api.client.event.lifecycle.v1.ClientTickEvents;
import net.fabricmc.fabric.api.client.keybinding.v1.KeyBindingHelper;
import net.fabricmc.fabric.api.client.rendering.v1.HudRenderCallback;
import net.minecraft.client.KeyMapping;
import net.minecraft.client.Minecraft;
import net.minecraft.network.chat.Component;
import org.lwjgl.glfw.GLFW;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * Einstiegspunkt des JenoXx Clients.
 * Features: HUD (FPS, Koordinaten, Blickrichtung), Zoom (halten), Fullbright (umschalten).
 */
public class JenoXxClient implements ClientModInitializer {
	public static final String MOD_ID = "jenoxxclient";
	public static final Logger LOGGER = LoggerFactory.getLogger(MOD_ID);

	private static final String CATEGORY = "category." + MOD_ID;

	private static KeyMapping zoomKey;
	private static KeyMapping fullbrightKey;
	private static KeyMapping hudKey;

	@Override
	public void onInitializeClient() {
		zoomKey = KeyBindingHelper.registerKeyBinding(new KeyMapping(
				"key." + MOD_ID + ".zoom", InputConstants.Type.KEYSYM, GLFW.GLFW_KEY_C, CATEGORY));
		fullbrightKey = KeyBindingHelper.registerKeyBinding(new KeyMapping(
				"key." + MOD_ID + ".fullbright", InputConstants.Type.KEYSYM, GLFW.GLFW_KEY_G, CATEGORY));
		hudKey = KeyBindingHelper.registerKeyBinding(new KeyMapping(
				"key." + MOD_ID + ".hud", InputConstants.Type.KEYSYM, GLFW.GLFW_KEY_H, CATEGORY));

		ClientTickEvents.END_CLIENT_TICK.register(JenoXxClient::onTick);
		HudRenderCallback.EVENT.register(HudOverlay::render);

		LOGGER.info("JenoXx Client geladen.");
	}

	private static void onTick(Minecraft mc) {
		while (hudKey.consumeClick()) {
			Settings.hudEnabled = !Settings.hudEnabled;
		}

		while (fullbrightKey.consumeClick()) {
			Settings.fullbright = !Settings.fullbright;
			if (mc.player != null) {
				mc.player.displayClientMessage(Component.literal(
						"Fullbright: " + (Settings.fullbright ? "an" : "aus")), true);
			}
		}

		Features.updateFullbright(mc, Settings.fullbright);
		Features.updateZoom(mc, zoomKey.isDown());
	}
}
