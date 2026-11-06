import { describe, expect, it } from "vitest";
import { changesBetween, readablePath } from "./diff";
import { autoLayout, positionsFor } from "./layout";
import {
  clone, collectedBefore, deleteNode, edgesOf, groupProblems, isEditable, listenerJumpWarnings, newFlowTemplate, renameNode, serialize,
  uniqueId, variablesOf,
  type Flow,
} from "./spec";

const flow = (): Flow => {
  const f = newFlowTemplate("demo", "Demo");
  f.nodes.details = {
    type: "collect",
    fields: [{ name: "full_name", builtin: "name" }, { name: "city", type: "string", description: "City" }],
    next: "goodbye",
  };
  const listen = f.nodes.listen;
  if (listen.type === "agent") listen.transitions!.push({ to: "details", when: "wants help", capture: { topic: { description: "Topic" } } });
  return f;
};

describe("spec helpers", () => {
  it("renames a node and every reference to it, keeping order", () => {
    const f = renameNode(flow(), "goodbye", "closing");
    expect(Object.keys(f.nodes)).toEqual(["listen", "closing", "details"]);
    expect(edgesOf(f).map((e) => `${e.from}->${e.to}`)).toEqual(["listen->closing", "listen->details", "details->closing"]);
    expect(renameNode(f, "listen", "start").start_node).toBe("start");
  });

  it("deletes a node with its incoming transitions; a collect step loses its next", () => {
    const f = deleteNode(flow(), "goodbye");
    expect(Object.keys(f.nodes)).toEqual(["listen", "details"]);
    expect(edgesOf(f).map((e) => `${e.from}->${e.to}`)).toEqual(["listen->details"]);
    expect((f.nodes.details as { next: string }).next).toBe("");
  });

  it("lists built-in and flow variables once each", () => {
    expect(variablesOf(flow()).map((v) => v.name)).toEqual([
      "caller_number", "dialed_number", "now_ist", "today_ist", "topic", "full_name", "city",
    ]);
  });

  it("groups problems by node", () => {
    const g = groupProblems(
      ["nodes.listen.transitions[0].to: 'x' is not a node", "persona: Field required", "nodes.ghost: oops", "nodes: needs an end"],
      ["listen", "goodbye"],
    );
    expect(g.byNode).toEqual({ listen: ["nodes.listen.transitions[0].to: 'x' is not a node"] });
    expect(g.general).toEqual(["persona: Field required", "nodes.ghost: oops", "nodes: needs an end"]);
  });

  it("knows when JSON can be shown on the canvas", () => {
    expect(isEditable(flow())).toBe(true);
    expect(isEditable({ nodes: [] })).toBe(false);
    expect(isEditable({ nodes: { a: { type: "finish" } } })).toBe(false);
    expect(isEditable(null)).toBe(false);
  });

  it("serializes without undefined fields", () => {
    const f = clone(flow());
    (f as { greeting?: string | null }).greeting = undefined;
    expect(serialize(f)).not.toContain("greeting");
    expect(uniqueId("step", ["step", "step_2"])).toBe("step_3");
  });
});

describe("layout", () => {
  it("places every node left to right along the transitions", () => {
    const p = autoLayout(flow());
    expect(Object.keys(p).sort()).toEqual(["details", "goodbye", "listen"]);
    expect(p.listen.x).toBeLessThan(p.details.x);
    expect(p.details.x).toBeLessThan(p.goodbye.x);
  });

  it("keeps saved positions and places new nodes to the right", () => {
    const p = positionsFor(flow(), { listen: { x: 0, y: 0 }, goodbye: { x: 500, y: 0 } });
    expect(p.listen).toEqual({ x: 0, y: 0 });
    expect(p.details.x).toBeGreaterThan(500);
  });
});

describe("changes between versions", () => {
  it("lists changed fields, added and removed nodes", () => {
    const before = flow();
    const after = clone(before);
    after.persona = "New persona";
    after.guardrails!.push("Be kind.");
    delete after.nodes.details;
    after.nodes.extra = { type: "end", instructions: "Bye" };
    const changes = changesBetween(before, after);
    expect(changes).toContainEqual({ path: "persona", kind: "changed", before: before.persona, after: "New persona" });
    expect(changes).toContainEqual({ path: "guardrails[1]", kind: "added", after: "Be kind." });
    expect(changes).toContainEqual({ path: "nodes.details", kind: "removed", before: "Collect details" });
    expect(changes).toContainEqual({ path: "nodes.extra", kind: "added", after: "End call" });
    expect(changes.some((c) => c.path.startsWith("nodes.extra.") || c.path.startsWith("nodes.details."))).toBe(false);
    expect(changesBetween(before, clone(before))).toEqual([]);
    expect(readablePath("nodes.listen.transitions[0].when")).toBe("listen › transitions 1 › when");
  });

  it("treats everything as added when nothing is live yet", () => {
    const changes = changesBetween(null, flow());
    expect(changes.filter((c) => c.kind !== "added")).toEqual([]);
    expect(changes.map((c) => c.path)).toContain("nodes.listen");
  });
});

describe("side listeners", () => {
  const withListeners = (): Flow => {
    const f = flow();
    f.settings = {
      ...f.settings,
      listeners: [
        { kind: "keyword_watch", name: "fraud_watch", words: ["fraud"], then: "go_to", to: "details" },
        { kind: "promise_to_pay", name: "promise_to_pay", steps: ["listen", "details"] },
      ],
    };
    return f;
  };

  it("offers a promise's values as variables", () => {
    const names = variablesOf(withListeners()).map((v) => v.name);
    expect(names).toContain("promised_date");
    expect(names).toContain("promised_amount");
    expect(variablesOf(flow()).map((v) => v.name)).not.toContain("promised_date");
  });

  it("follows a renamed step, and stops listening on a deleted one", () => {
    const renamed = renameNode(withListeners(), "details", "ask_details").settings!.listeners!;
    expect(renamed[0]).toMatchObject({ to: "ask_details" });
    expect(renamed[1].steps).toEqual(["listen", "ask_details"]);
    const deleted = deleteNode(withListeners(), "details").settings!.listeners!;
    expect(deleted[1].steps).toEqual(["listen"]);
    // Its only step deleted: left for the editor to fix, not widened to every step.
    const only = withListeners();
    only.settings!.listeners![1].steps = ["details"];
    expect(deleteNode(only, "details").settings!.listeners![1].steps).toEqual(["details"]);
  });

  it("shows a reviewer each listener as one change, by name", () => {
    const before = withListeners();
    const after = clone(before);
    after.settings!.listeners!.reverse(); // reordering is no change
    expect(changesBetween(before, after)).toEqual([]);
    const fraud = after.settings!.listeners!.find((l) => l.name === "fraud_watch")!;
    if (fraud.kind === "keyword_watch") fraud.words.push("धोखाधड़ी");
    after.settings!.listeners!.push({ kind: "secret_guard", name: "secret_guard" });
    const changes = changesBetween(before, after);
    expect(changes).toContainEqual({ path: "settings.listeners.fraud_watch.words[1]", kind: "added", after: "धोखाधड़ी" });
    expect(changes).toContainEqual({ path: "settings.listeners.secret_guard", kind: "added", after: "Secret guard secret_guard on every step" });
    expect(changes).toHaveLength(2);
    const removed = changesBetween(before, flow());
    expect(removed).toContainEqual({
      path: "settings.listeners.fraud_watch",
      kind: "removed",
      before: "Keyword watch fraud_watch on every step: “fraud” → go to details",
    });
  });
});

describe("listener jump warnings", () => {
  // ask (collect loan_last4) -> talk (agent) -> check (agent, tool needs loan_last4)
  //                                          -> dispute (API, needs dispute_date, captured on the way) -> done
  const loanFlow = (): Flow => ({
    schema_version: 1,
    flow_id: "loans",
    name: "Loans",
    persona: "A loan helpline.",
    start_node: "ask",
    settings: {},
    nodes: {
      ask: { type: "collect", fields: [{ name: "loan_last4", type: "string", description: "Last 4 digits" }], next: "talk" },
      talk: {
        type: "agent",
        instructions: "Help.",
        transitions: [
          { to: "check", when: "A debit question." },
          { to: "dispute", when: "Dispute.", capture: { dispute_date: { description: "Date" } } },
        ],
      },
      check: {
        type: "agent",
        instructions: "Check debits.",
        tools: [{ tool: "core.recent_debits", inputs: { loan_last4: "{{loan_last4}}", phone: "{{caller_number}}" } }],
        transitions: [{ to: "done", when: "Done." }],
      },
      dispute: {
        type: "api",
        tool: "core.raise_dispute",
        inputs: { loan: "{{loan_last4}}", date: "{{dispute_date}}" },
        save: ["reference"],
        next: "done",
        on_error: "done",
      },
      done: { type: "end", instructions: "Close." },
    },
  });
  const watch = (to: string, steps?: string[]): Flow => {
    const f = loanFlow();
    f.settings = { listeners: [{ kind: "keyword_watch", name: "fraud_watch", words: ["fraud"], then: "go_to", to, ...(steps ? { steps } : {}) }] };
    return f;
  };

  it("knows what every route into a step has collected", () => {
    const known = collectedBefore(loanFlow());
    expect([...known.ask!]).toEqual([]);
    expect([...known.talk!]).toEqual(["loan_last4"]);
    expect([...known.dispute!].sort()).toEqual(["dispute_date", "loan_last4"]);
    // done is reached when the dispute fails too: "reference" isn't certain there.
    expect(known.done!.has("reference")).toBe(false);
  });

  it("warns when an API step would run without its values", () => {
    const [w] = listenerJumpWarnings(watch("dispute"));
    expect(w.missing.sort()).toEqual(["dispute_date", "loan_last4"]);
    expect(w.steps).toEqual(["ask", "talk", "check", "done"]); // done: also reached when the dispute fails
    expect(w.text).toContain("which calls core.raise_dispute");
    expect(w.text).toContain("so the call would fail and go to done");
  });

  it("warns about a conversation step's tools only where their values may be missing", () => {
    const [w] = listenerJumpWarnings(watch("check"));
    expect(w.steps).toEqual(["ask"]); // built-ins like {{caller_number}} are always there
    expect(w.missing).toEqual(["loan_last4"]);
    expect(w.text).toContain("whose tools need {{loan_last4}}");
    expect(listenerJumpWarnings(watch("check", ["talk", "dispute"]))).toEqual([]);
    expect(listenerJumpWarnings(watch("talk"))).toEqual([]); // no tools: nothing to warn about
  });

  it("checks a time limit's and an off-topic guard's step too", () => {
    const f = loanFlow();
    f.settings = {
      listeners: [
        { kind: "time_limit", name: "time_limit", then: "go_to", to: "dispute" },
        { kind: "off_topic", name: "off_topic", steps: ["check"], then: "go_to", to: "check" },
      ],
    };
    const warnings = listenerJumpWarnings(f);
    expect(warnings.map((w) => [w.name, w.steps])).toEqual([["time_limit", ["ask", "talk", "check", "done"]]]);
    expect(changesBetween(null, f)[0]).toEqual({
      path: "settings.listeners.time_limit",
      kind: "added",
      after: "Call time limit time_limit: 30 minutes (warning 2 before) → go to dispute",
    });
  });
});
