package vm

// prelude defines the TypeScript/JS-facing capability surface on top of the
// native primitives (__af_op, __af_now, __af_mem_cap).
const prelude = `(function(global) {
  "use strict";

  // Soft allocation guards honoring vm.State.SetMemoryCap(capBytes).
  const origRepeat = String.prototype.repeat;
  String.prototype.repeat = function(count) {
    const cap = __af_mem_cap();
    if (cap > 0) {
      const n = Math.floor(Number(count));
      if (n > 0 && this.length > 0 && n * this.length > cap) {
        throw new RangeError("out of memory");
      }
    }
    return origRepeat.call(this, count);
  };

  const origJoin = Array.prototype.join;
  Array.prototype.join = function(sep) {
    const cap = __af_mem_cap();
    if (cap > 0 && this.length > 0) {
      const s = sep === undefined ? "," : String(sep);
      let total = Math.max(0, this.length - 1) * s.length;
      for (let i = 0; i < this.length; i++) {
        const item = this[i];
        if (item !== undefined && item !== null) {
          total += typeof item === "string" ? item.length : String(item).length;
        }
        if (total > cap) {
          throw new RangeError("out of memory");
        }
      }
    }
    return origJoin.call(this, sep);
  };

  // JSON convenience alias alongside standard global JSON.
  const json = {
    encode: function(v) {
      const out = JSON.stringify(v);
      if (out === undefined) {
        throw new TypeError("json: cannot encode " + typeof v);
      }
      return out;
    },
    decode: function(s) {
      if (typeof s !== "string") {
        throw new TypeError("json: expected string");
      }
      return JSON.parse(s);
    },
  };
  global.json = json;

  // Native primitive: __af_op(request_json) -> [response_json, ok]
  function op(req) {
    const res = __af_op(json.encode(req));
    const resp = res[0];
    const ok = res[1];
    if (!ok) {
      let msg = resp;
      try {
        const v = JSON.parse(resp);
        if (typeof v === "string") {
          msg = v;
        }
      } catch (_) {}
      throw new Error(String(msg));
    }
    return JSON.parse(resp);
  }

  function callOp(req) {
    return global.af.op(req);
  }

  const af = { op: op };
af.version = 2;
  global.af = af;

  const session = {
    inbox: function() { return callOp({ type: "inbox" }); },
    state: {
      set: function(key, value) {
        return callOp({ type: "session.state.set", key: key, value: value });
      },
      get: function(key) {
        return callOp({ type: "session.state.get", key: key });
      },
      delete: function(key) {
        return callOp({ type: "session.state.delete", key: key });
      },
      list: function() {
        return callOp({ type: "session.state.list" });
      },
    },
    send: function(text, opts) {
      const req = { type: "send", text: text };
      if (opts && opts.attachments) req.attachments = opts.attachments;
      callOp(req);
    },
    reply: function(t) {
      t = t || {};
      const req = {
        type: "reply",
        text: t.text,
        finish_reason: t.finish_reason,
        reasoning: t.reasoning !== undefined && t.reasoning !== null ? t.reasoning : t.thinking,
      };
      if (t.tool_calls && t.tool_calls.length > 0) req.tool_calls = t.tool_calls;
      if (t.usage) req.usage = t.usage;
      return callOp(req);
    },
    send_delta: function(text) {
      return callOp({ type: "send.delta", text: text });
    },
    push: function(channel, reply_to, text, opts) {
      const req = { type: "session.push", channel: channel, reply_to: reply_to, text: text };
      if (opts && opts.attachments) req.attachments = opts.attachments;
      return callOp(req);
    },
    push_user: function(uuid, text) {
      return callOp({ type: "session.push_user", address: "user:" + uuid, text: text });
    },
    exit: function() {
      callOp({ type: "session.exit" });
    },
  };
  global.session = session;

  function mklog(level) {
    return function(msg) { callOp({ type: "log", level: level, msg: String(msg) }); };
  }
  global.log = {
    debug: mklog("debug"),
    info: mklog("info"),
    warn: mklog("warn"),
    error: mklog("error"),
  };

  global.time = {
    sleep: function(seconds) { callOp({ type: "sleep", seconds: seconds }); },
  };

  const agent = {
    info: function() { return callOp({ type: "agent.info" }); },
    config: function() { return callOp({ type: "agent.config" }); },
    send: function(address, payload) {
      return callOp({ type: "agent.send", address: address, payload: payload || {} });
    },
    request: function(address, payload, timeout) {
      return callOp({
        type: "agent.request",
        address: address,
        payload: payload || {},
        timeout: timeout !== undefined && timeout !== null ? timeout : 30,
      });
    },
    reply: function(request_id, payload) {
      return callOp({ type: "agent.reply", request: request_id, payload: payload || {} });
    },
    list: function() { return callOp({ type: "agent.list" }); },
    spawn: function(profile, spec) {
      return callOp({ type: "agent.spawn", profile: profile, spec: spec || {} });
    },
  };
  global.agent = agent;

  global.scheduler = {
    every: function(interval, _opts) {
      return callOp({ type: "scheduler.every", interval: interval });
    },
    after: function(delay, _opts) {
      return callOp({ type: "scheduler.after", delay: delay });
    },
    cron: function(expr, _opts) {
      return callOp({ type: "scheduler.cron", cron: expr });
    },
    cancel: function(timer_id) {
      return callOp({ type: "scheduler.cancel", timer_id: timer_id });
    },
  };

  const store = {
    put: function(table, key, value, opts) {
      opts = opts || {};
      return callOp({ type: "store.put", table: table, key: key, value: value, ttl: opts.ttl, vector: opts.vector });
    },
    get: function(table, key) {
      return callOp({ type: "store.get", table: table, key: key });
    },
    query: function(table, query) {
      const q = Object.assign({}, query || {}, { table: table });
      return callOp({ type: "store.query", query: q });
    },
    delete: function(table, key) {
      return callOp({ type: "store.delete", table: table, key: key });
    },
    scopes: function(table) {
      return callOp({ type: "store.scopes", table: table });
    },
  };
  global.store = store;

  global.user = {
    current: function() { return callOp({ type: "user.current" }); },
    usage: function() { return callOp({ type: "user.usage" }); },
    has_credential: function(service) {
      return callOp({ type: "user.has_credential", service: service });
    },
    get: function(id) { return callOp({ type: "user.get", user_id: id }); },
    list: function() { return callOp({ type: "user.list" }); },
  };

  global.memory = {
    write: function(record) {
      const targets = global.memory_routing_table(record) || [];
      const info = agent.info();
      const mem = (info && info.memory) || {};
      const embedModel = mem.embed_model;
      const stores = mem.stores || {};
      for (let i = 0; i < targets.length; i++) {
        const t = targets[i];
        const opts = { ttl: t.ttl };
        if (embedModel && embedModel !== "") {
          const s = stores[t.store];
          const canVector = Boolean(s && Array.isArray(s.features) && s.features.indexOf("vector") >= 0);
          if (canVector) {
            let text = t.value;
            if (text && typeof text === "object") {
              text = text.text !== undefined && text.text !== null ? text.text : json.encode(text);
            }
            const atts = record && record.attachments;
            if (Array.isArray(atts) && atts.length > 0) {
              const parts = [];
              if (typeof text === "string" && text.length > 0) {
                parts.push(text);
              }
              for (let j = 0; j < atts.length; j++) {
                parts.push(atts[j]);
              }
              const r = global.llm.embed(parts, { model: embedModel, merged: true });
              if (r && Array.isArray(r.vectors) && r.vectors.length > 0) {
                opts.vector = r.vectors[0];
              }
            } else if (typeof text === "string" && text.length > 0) {
              const r = global.llm.embed([text], { model: embedModel });
              if (r && Array.isArray(r.vectors) && r.vectors.length > 0) {
                opts.vector = r.vectors[0];
              }
            }
          }
        }
        store.put(t.store, t.key, t.value, opts);
      }
      return true;
    },
    recall: function(query, opts) {
      const info = agent.info();
      const recall = info && info.memory && info.memory.recall;
      if (recall === "plugin:semantic" && typeof global.memory_semantic_recall_handler === "function") {
        return global.memory_semantic_recall_handler(query, opts);
      }
      return global.memory_recall_handler(query, opts);
    },
  };

  function goToolsList() {
    return callOp({ type: "tools.list" });
  }
  function goToolsRun(name, args, opts) {
    const req = { type: "tools.run", tool: name, args: args || {} };
    if (opts && opts.confirmed) {
      req.confirmed = true;
      req.confirm_id = opts.confirm_id;
    }
    return callOp(req);
  }

  const scriptTools = Object.create(null);

  global.tool = {
    def: function(spec) {
      if (!spec || typeof spec !== "object" || Array.isArray(spec)) {
        throw new TypeError("tool.def: spec must be an object");
      }
      if (typeof spec.name !== "string" || spec.name.length === 0) {
        throw new TypeError("tool.def: spec.name must be a non-empty string");
      }
      if (typeof spec.handler !== "function") {
        throw new TypeError("tool.def: spec.handler must be a function");
      }
      if (spec.params !== undefined && spec.params !== null && (typeof spec.params !== "object" || Array.isArray(spec.params))) {
        throw new TypeError("tool.def: spec.params must be a JSON-schema object");
      }
      scriptTools[spec.name] = spec;
    },
  };

  function toolOverrideParams(params, ovparams) {
    const base = params || { type: "object", properties: {} };
    if (!ovparams) {
      return base;
    }
    const out = Object.assign({}, base);
    const props = Object.assign({}, base.properties || {});
    const ovKeys = Object.keys(ovparams);
    for (let i = 0; i < ovKeys.length; i++) {
      const pname = ovKeys[i];
      const po = ovparams[pname];
      const declared = props[pname];
      if (po && po.description && declared && typeof declared === "object") {
        props[pname] = Object.assign({}, declared, { description: po.description });
      }
    }
    out.properties = props;
    return out;
  }

  function toolDeclared(names) {
    const res = callOp({ type: "tools.declared", tool_names: names }) || {};
    return [res.visible || {}, res.overrides || {}];
  }

  global.tools = {
    list: function() {
      const names = Object.keys(scriptTools);
      let visible = {};
      let overrides = {};
      if (names.length > 0) {
        const pair = toolDeclared(names);
        visible = pair[0];
        overrides = pair[1];
      }

      const out = [];
      const nativeList = goToolsList() || [];
      for (let i = 0; i < nativeList.length; i++) {
        const def = nativeList[i];
        const f = def && def["function"];
        if (!(f && visible[f.name])) {
          out.push(def);
        }
      }
      for (let i = 0; i < names.length; i++) {
        const name = names[i];
        if (visible[name]) {
          const spec = scriptTools[name];
          const ov = overrides[name];
          out.push({
            type: "function",
            "function": {
              name: name,
              description: (ov && ov.description) || spec.description || "",
              parameters: toolOverrideParams(spec.params, ov && ov.params),
            },
          });
        }
      }
      return out;
    },
    run: function(name, args, opts) {
      const spec = scriptTools[name];
      if (!spec) {
        return goToolsRun(name, args, opts);
      }
      const pair = toolDeclared([name]);
      const visible = pair[0];
      if (!visible[name]) {
        return goToolsRun(name, args, opts);
      }
      try {
        const res = spec.handler(args || {});
        if (res === undefined || res === null) {
          return { ok: true, tool: name };
        }
        return res;
      } catch (err) {
        return {
          ok: false,
          tool: name,
          error: err instanceof Error ? err.message : String(err),
        };
      }
    },
  };

  function shellJob(kind, command, opts) {
    opts = opts || {};
    return callOp({
      type: kind,
      cmd: command,
      project: opts.project,
      environment: opts.environment,
      workdir: opts.workdir,
      env: opts.env,
      timeout: opts.timeout,
    });
  }

  global.shell = {
    spawn: function(opts) {
      opts = opts || {};
      return callOp({
        type: "shell.spawn",
        image: opts.image,
        workdir: opts.workdir,
        env: opts.env,
        network: opts.network,
        mem_limit: opts.mem_limit,
        cpu_limit: opts.cpu_limit,
        provider: opts.provider,
        host: opts.host,
        user: opts.user,
        password: opts.password,
        key_file: opts.key_file,
        volumes: opts.volumes,
        project: opts.project,
        ref: opts.ref,
        scratch_mount: opts.scratch_mount,
        shell_opts: opts.shell_opts,
        environment: opts.environment,
      });
    },
    exec: function(handle_id, command, opts) {
      opts = opts || {};
      return callOp({ type: "shell.exec", shell_handle: handle_id, cmd: command, timeout: opts.timeout });
    },
    run: function(command, opts) { return shellJob("shell.run", command, opts); },
    start: function(command, opts) { return shellJob("shell.start", command, opts); },
    status: function(job_id) { return callOp({ type: "shell.status", job_id: job_id }); },
    cancel: function(job_id) { return callOp({ type: "shell.cancel", job_id: job_id }); },
    write: function(handle_id, filepath, content) {
      return callOp({ type: "shell.write", shell_handle: handle_id, path: filepath, content: content });
    },
    destroy: function(handle_id) {
      return callOp({ type: "shell.destroy", shell_handle: handle_id });
    },
  };

  global.ssh = {
    spawn: function(opts) {
      opts = opts || {};
      return callOp({
        type: "ssh.spawn",
        workdir: opts.workdir,
        env: opts.env,
        host: opts.host,
        user: opts.user,
        password: opts.password,
        key_file: opts.key_file,
        volumes: opts.volumes,
        project: opts.project,
        ref: opts.ref,
        scratch_mount: opts.scratch_mount,
        shell_opts: opts.shell_opts,
      });
    },
    exec: function(handle_id, command, opts) {
      opts = opts || {};
      return callOp({ type: "ssh.exec", shell_handle: handle_id, cmd: command, timeout: opts.timeout });
    },
    read: function(handle_id, filepath) {
      return callOp({ type: "ssh.read", shell_handle: handle_id, path: filepath });
    },
    write: function(handle_id, filepath, content) {
      return callOp({ type: "ssh.write", shell_handle: handle_id, path: filepath, content: content });
    },
    close: function(handle_id) {
      return callOp({ type: "ssh.close", shell_handle: handle_id });
    },
  };

  const httpNS = {
    request: function(opts) {
      opts = opts || {};
      return callOp({
        type: "http.request",
        method: opts.method || "GET",
        url: opts.url,
        headers: opts.headers,
        body: opts.body,
        query_params: opts.query,
        json: opts.json,
        timeout: opts.timeout,
        auth: opts.auth,
        save_to: opts.save_to,
      });
    },
    get: function(url, opts) {
      return httpNS.request(Object.assign({}, opts || {}, { method: "GET", url: url }));
    },
    post: function(url, body, opts) {
      return httpNS.request(Object.assign({}, opts || {}, { method: "POST", url: url, body: body }));
    },
  };
  global.http = httpNS;

  const WDAY = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];
  const MONTH = [
    "January", "February", "March", "April", "May", "June",
    "July", "August", "September", "October", "November", "December",
  ];

  function dateFields(t) {
    const d = new Date(Math.trunc(t) * 1000);
    const year = d.getUTCFullYear();
    const month = d.getUTCMonth() + 1;
    const day = d.getUTCDate();
    const hour = d.getUTCHours();
    const min = d.getUTCMinutes();
    const sec = d.getUTCSeconds();
    const dow = d.getUTCDay();
    const jan1 = Date.UTC(year, 0, 1);
    const cur = Date.UTC(year, month - 1, day);
    const yday = Math.floor((cur - jan1) / 86400000) + 1;
    return {
      year: year,
      month: month,
      day: day,
      hour: hour,
      min: min,
      sec: sec,
      wday: dow + 1,
      yday: yday,
      isdst: false,
    };
  }

  function pad2(n) { return (n < 10 ? "0" : "") + n; }
  function pad3(n) { return (n < 10 ? "00" : n < 100 ? "0" : "") + n; }
  function pad4(n) {
    const s = String(n);
    if (s.length >= 4) return s;
    return ("0000" + s).slice(-4);
  }

  function strftime(f, t) {
    const dt = dateFields(t);
    return f.replace(/%([a-zA-Z%])/g, function(_, c) {
      if (c === "Y") return pad4(dt.year);
      if (c === "y") return pad2(dt.year % 100);
      if (c === "m") return pad2(dt.month);
      if (c === "d") return pad2(dt.day);
      if (c === "e") return (dt.day < 10 ? " " : "") + dt.day;
      if (c === "H") return pad2(dt.hour);
      if (c === "I") return pad2(((dt.hour + 11) % 12) + 1);
      if (c === "M") return pad2(dt.min);
      if (c === "S") return pad2(dt.sec);
      if (c === "p") return dt.hour < 12 ? "AM" : "PM";
      if (c === "A") return WDAY[dt.wday - 1];
      if (c === "a") return WDAY[dt.wday - 1].slice(0, 3);
      if (c === "B") return MONTH[dt.month - 1];
      if (c === "b") return MONTH[dt.month - 1].slice(0, 3);
      if (c === "j") return pad3(dt.yday);
      if (c === "w") return String(dt.wday - 1);
      if (c === "u") return String(dt.wday === 1 ? 7 : dt.wday - 1);
      if (c === "Z") return "UTC";
      if (c === "%") return "%";
      return "%" + c;
    });
  }

  global.os = {
    env: function(name) { return callOp({ type: "os.env", name: name }); },
    time: function() {
      if (arguments.length > 0) {
        throw new Error("os.time: takes no arguments (UTC Unix seconds)");
      }
      return __af_now();
    },
    date: function(format, t) {
      if (typeof format === "number" && t === undefined) {
        t = format;
        format = undefined;
      }
      if (t === undefined || t === null) {
        t = __af_now();
      }
      if (typeof t !== "number" || Number.isNaN(t)) {
        throw new TypeError("os.date: time must be a number (Unix seconds)");
      }
      if (format === undefined || format === null || format === "" || format === "*t" || format === "!*t") {
        return dateFields(t);
      }
      if (typeof format !== "string") {
        throw new TypeError("os.date: format must be a string");
      }
      if (format.charAt(0) === "!") format = format.slice(1);
      if (format === "*t") return dateFields(t);
      return strftime(format, t);
    },
  };

  global.runtime = {
    triggers: function() { return callOp({ type: "runtime.triggers" }); },
  };

  global.credential = {
    get: function(name) { return callOp({ type: "credential.get", cred_name: name }); },
  };

  global.mail = {
    imap_fetch: function(opts) {
      opts = opts || {};
      return callOp({
        type: "mail.imap.fetch",
        mail_host: opts.host,
        mail_port: opts.port,
        mail_user: opts.user,
        mailbox: opts.mailbox,
        unseen: opts.unseen,
        limit: opts.limit,
        auth: opts.auth,
      });
    },
    smtp_send: function(opts) {
      opts = opts || {};
      return callOp({
        type: "mail.smtp.send",
        mail_host: opts.host,
        mail_port: opts.port,
        mail_user: opts.user,
        mail_from: opts.from,
        mail_to: opts.to,
        subject: opts.subject,
        text_body: opts.text_body !== undefined && opts.text_body !== null ? opts.text_body : opts.body,
        auth: opts.auth,
      });
    },
  };

  function normToolChoice(v) {
    if (v && typeof v === "object") {
      const name = v.name || (v["function"] && v["function"].name);
      if (typeof name === "string" && name !== "") {
        return "function:" + name;
      }
      throw new TypeError('tool_choice: an object must name a function, e.g. { name: "lookup" }');
    }
    return v;
  }

  function normStop(v) {
    if (typeof v === "string") return [v];
    if (Array.isArray(v) && v.length === 0) return undefined;
    return v;
  }

  function withOpts(req, opts) {
    if (opts && typeof opts === "object") {
      const keys = Object.keys(opts);
      for (let i = 0; i < keys.length; i++) {
        req[keys[i]] = opts[keys[i]];
      }
    }
    req.tool_choice = normToolChoice(req.tool_choice);
    req.stop = normStop(req.stop);
    return req;
  }

  global.llm = {
    chat: function(messages, opts) {
      return callOp(withOpts({ type: "llm.chat", messages: messages }, opts));
    },
    forward: function(opts) {
      const req = { type: "llm.forward" };
      if (opts) {
        req.model = opts.model;
        req.messages = opts.messages;
        req.prepend = opts.prepend;
        req.append = opts.append;
        req.tools = opts.tools;
        req.max_tokens_cap = opts.max_tokens_cap;
        const p = opts.params || {};
        req.temperature = p.temperature;
        req.max_tokens = p.max_tokens;
        req.top_p = p.top_p;
        req.stop = normStop(p.stop);
        req.seed = p.seed;
        req.response_format = p.response_format;
        req.parallel_tool_calls = p.parallel_tool_calls;
        req.tool_choice = normToolChoice(p.tool_choice);
        req.thinking = p.thinking;
      }
      return callOp(req);
    },
    embed: function(inputs, opts) {
      const arr = Array.isArray(inputs) ? inputs : [inputs];
      const parts = [];
      for (let i = 0; i < arr.length; i++) {
        const item = arr[i];
        if (typeof item === "string") {
          parts.push({ type: "text", text: item });
        } else if (item && typeof item === "object") {
          parts.push(item);
        }
      }
      return callOp(withOpts({ type: "llm.embed", inputs: parts }, opts));
    },
    rerank: function(query, documents, opts) {
      return callOp(withOpts({ type: "llm.rerank", text: query, documents: documents }, opts));
    },
    stream: function(messages, opts) {
      const h = callOp(withOpts({ type: "llm.stream.open", messages: messages }, opts));
      let done = false;
      function close() {
        if (done) return;
        done = true;
        callOp({ type: "llm.stream.close", stream: h.id });
      }
      const iter = {
        close: close,
        next: function() {
          if (done) return { value: undefined, done: true };
          const r = callOp({ type: "llm.stream.next", stream: h.id });
          if (r && r.done) {
            close();
            return { value: undefined, done: true };
          }
          return { value: r && r.delta, done: false };
        },
        return: function() {
          close();
          return { value: undefined, done: true };
        },
      };
      iter[Symbol.iterator] = function() { return iter; };
      return iter;
    },
  };

  global.openai = {
    request: function() { return callOp({ type: "openai.request" }); },
  };

  global.files = {
    put: function(project, path, content, opts) {
      opts = opts || {};
      const req = { type: "files.put", project: project, path: path };
      if (content && typeof content === "object") {
        req.data = content.data;
        req.handle = content.handle;
        req.mime = content.mime || opts.mime;
      } else {
        req.content = content;
        req.mime = opts.mime;
      }
      return callOp(req);
    },
    read: function(project, path) {
      return callOp({ type: "files.read", project: project, path: path });
    },
    list: function(project, prefix) {
      return callOp({ type: "files.list", project: project, path: prefix || "" });
    },
    projects: function() {
      return callOp({ type: "files.projects" });
    },
    delete: function(project, path) {
      return callOp({ type: "files.delete", project: project, path: path });
    },
    commit: function(project, opts) {
      opts = opts || {};
      return callOp({ type: "files.commit", project: project, ref: opts.ref, commit_msg: opts.message });
    },
    checkout: function(project, ref) {
      return callOp({ type: "files.checkout", project: project, ref: ref });
    },
    scratch: {
      put: function(name, content, opts) {
        opts = opts || {};
        const req = { type: "files.scratch.put", path: name };
        if (content && typeof content === "object") {
          req.data = content.data;
          req.handle = content.handle;
          req.mime = content.mime || opts.mime;
        } else {
          req.content = content;
          req.mime = opts.mime;
        }
        return callOp(req);
      },
      read: function(name) {
        return callOp({ type: "files.scratch.read", path: name });
      },
      list: function() {
        return callOp({ type: "files.scratch.list" });
      },
    },
  };

  // External APIs (api.list, api.call, api.batch + api.<pkg>.<action>(args) proxy)
  const apiBase = Object.freeze({
    list: function() {
      return callOp({ type: "api.list" });
    },
    call: function(pkg, action, args) {
      return callOp({
        type: "api.call",
        api: String(pkg),
        action: String(action),
        args: args || {},
      });
    },
    batch: function(calls) {
      const list = Array.isArray(calls) ? calls : [];
      const norm = [];
      for (let i = 0; i < list.length; i++) {
        const c = list[i] || {};
        norm.push({
          api: String(c.api || ""),
          action: String(c.action || ""),
          args: c.args || {},
        });
      }
      return callOp({ type: "api.batch", calls: norm });
    },
  });

  global.api = new Proxy(apiBase, {
    get: function(target, prop, receiver) {
      if (typeof prop === "symbol" || Object.prototype.hasOwnProperty.call(target, prop)) {
        return Reflect.get(target, prop, receiver);
      }
      if (prop === "then" || prop === "toJSON") {
        return undefined;
      }
      const pkg = String(prop);
      return new Proxy({}, {
        get: function(_t, action) {
          if (typeof action === "symbol" || action === "then" || action === "toJSON") {
            return undefined;
          }
          const act = String(action);
          return function(args) {
            return apiBase.call(pkg, act, args);
          };
        },
        set: function() {
          throw new TypeError("api." + pkg + " is read-only");
        },
        defineProperty: function() {
          throw new TypeError("api." + pkg + " is read-only");
        },
        deleteProperty: function() {
          throw new TypeError("api." + pkg + " is read-only");
        },
      });
    },
    set: function() {
      throw new TypeError("api is read-only");
    },
    defineProperty: function() {
      throw new TypeError("api is read-only");
    },
    deleteProperty: function() {
      throw new TypeError("api is read-only");
    },
  });
})(globalThis);
`

// LoadBase installs the TypeScript/JS prelude into the state.
func (s *State) LoadBase() error {
	return s.Eval("@prelude", prelude)
}
