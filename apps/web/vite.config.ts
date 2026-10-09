import { defineConfig, loadEnv } from "vite";
import react from "@vitejs/plugin-react";

// SINTHMUX_HUB_TARGET lets end-to-end tests point the dev server at a test Hub.
export default defineConfig(({ mode }) => {
  const hub = loadEnv(mode, ".", "SINTHMUX_").SINTHMUX_HUB_TARGET || "http://127.0.0.1:8090";
  return {
    plugins: [react()],
    server: {
      port: 5173,
      proxy: {
        "/api": hub,
        "/ws": { target: hub.replace(/^http/, "ws"), ws: true },
        "/health": hub,
        "/install": hub,
        "/downloads": hub
      }
    }
  };
});
