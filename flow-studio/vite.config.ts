import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// `npm run dev` (the local setup in dev/, or on its own): the same paths Caddy
// serves on EC2, so the app works unchanged. /api/v1/* goes to the config
// service (its /internal API is never exposed), /auth/realms and
// /auth/resources to Keycloak. Defaults suit `npm run dev` on your Mac next to
// `make dev`; the local setup's studio container sets the container names.
const API = process.env.API_PROXY_TARGET ?? "http://localhost:8080";
const KEYCLOAK = process.env.KEYCLOAK_PROXY_TARGET ?? "http://localhost:8180";

export default defineConfig({
  plugins: [react()],
  build: { outDir: "dist", sourcemap: false },
  server: {
    port: 3000,
    strictPort: true,
    proxy: {
      "/api/v1": { target: API, rewrite: (path) => path.replace(/^\/api/, "") },
      "/auth/realms": { target: KEYCLOAK },
      "/auth/resources": { target: KEYCLOAK },
    },
    // Docker Desktop doesn't always pass file changes into containers; the local
    // setup polls instead (VITE_POLL=1).
    watch: process.env.VITE_POLL ? { usePolling: true, interval: 300 } : undefined,
  },
  test: { environment: "node", include: ["src/**/*.test.ts"] },
});
