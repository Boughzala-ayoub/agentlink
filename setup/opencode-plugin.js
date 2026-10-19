// agentlink for opencode 2.x. When another AI agent messages one of this
// opencode's sessions, it starts a turn there with agentlink's wake line; the
// message itself arrives only through the agentlink_inbox tool.
// `agentlink setup` installed this file and `agentlink uninstall` removes it.
import { spawn } from "node:child_process";
import { createInterface } from "node:readline";

const EXE = "__AGENTLINK_EXE__";

export default {
  id: "agentlink",
  async setup(ctx) {
    const feed = spawn(EXE, ["opencode-feed"], { stdio: ["pipe", "pipe", "ignore"], windowsHide: true });
    feed.on("error", () => {});
    const busy = new Set();
    const known = new Set();
    const woke = new Set(); // sessions whose next run is one agentlink started, not a person
    const tell = (o) => { try { feed.stdin.write(JSON.stringify(o) + "\n"); } catch {} };
    const see = async (sessionID, human) => {
      let directory = "";
      if (!known.has(sessionID)) {
        known.add(sessionID);
        try {
          const s = await ctx.session.get({ sessionID });
          directory = (s?.data ?? s)?.location?.directory ?? "";
        } catch {}
      }
      tell({ session: sessionID, directory, human });
    };
    createInterface({ input: feed.stdout }).on("line", (line) => {
      let w;
      try { w = JSON.parse(line); } catch { return; }
      woke.add(w.session);
      // An idle session starts a run at once; a busy one gets it when its run ends.
      ctx.session.prompt({ sessionID: w.session, text: w.text, delivery: busy.has(w.session) ? "queue" : "steer" }).catch(() => woke.delete(w.session));
    });
    const controller = new AbortController();
    void (async () => {
      try {
        for await (const e of ctx.event.subscribe({ signal: controller.signal })) {
          const id = e.data?.sessionID;
          if (!id) continue;
          if (e.type === "session.execution.started") { busy.add(id); see(id, !woke.delete(id)); }
          else if (/^session\.execution\.(succeeded|failed|interrupted)$/.test(e.type)) busy.delete(id);
          else if (e.type === "session.created" && !known.has(id)) see(id, false);
        }
      } catch { /* aborted on dispose */ }
    })();
    return () => { controller.abort(); feed.kill(); };
  },
};
