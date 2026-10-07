// Cloudflare entry for the relay. The Go relay runs in a container; this Worker sends every request,
// WebSocket upgrades included, to that one container and wakes it when it has gone to sleep.
import { Container, getContainer } from "@cloudflare/containers";
import { env } from "cloudflare:workers";

export class Relay extends Container {
	defaultPort = 8080;
	// Unread messages live in Postgres, so sleeping when idle loses nothing; the next request restarts it.
	sleepAfter = "30m";
	envVars = {
		DATABASE_URL: env.DATABASE_URL ?? "",
		PUBLIC_NETWORK_URL: env.PUBLIC_NETWORK_URL,
		LISTEN_ADDR: "0.0.0.0",
		PORT: "8080",
	};
}

export default {
	fetch(request, env) {
		// One named instance: the relay keeps live WebSocket and long-poll state in memory.
		return getContainer(env.RELAY, "main").fetch(request);
	},
};
