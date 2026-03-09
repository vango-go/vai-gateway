function escapeHTML(value) {
  return String(value ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}

function sanitizeURL(raw) {
  try {
    const url = new URL(String(raw ?? ""), window.location.href);
    if (url.protocol === "http:" || url.protocol === "https:") {
      return url.href;
    }
  } catch {}
  return "#";
}

function renderInlineMarkdown(input) {
  let html = escapeHTML(input);

  html = html.replace(
    /\[([^\]]+)\]\(([^)\s]+)\)/g,
    (_, label, href) =>
      `<a href="${escapeHTML(sanitizeURL(href))}" target="_blank" rel="noreferrer">${escapeHTML(label)}</a>`,
  );
  html = html.replace(/`([^`]+)`/g, "<code>$1</code>");
  html = html.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
  html = html.replace(/__([^_]+)__/g, "<strong>$1</strong>");
  html = html.replace(/(^|[\s(])\*([^*]+)\*(?=[\s).,!?:;]|$)/g, "$1<em>$2</em>");
  html = html.replace(/(^|[\s(])_([^_]+)_(?=[\s).,!?:;]|$)/g, "$1<em>$2</em>");

  return html;
}

function renderMarkdown(markdown) {
  const source = String(markdown ?? "").replace(/\r\n/g, "\n");
  if (!source.trim()) {
    return "";
  }

  const lines = source.split("\n");
  const out = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];
    const trimmed = line.trim();

    if (!trimmed) {
      i += 1;
      continue;
    }

    if (trimmed.startsWith("```")) {
      const lang = trimmed.slice(3).trim();
      const code = [];
      i += 1;
      while (i < lines.length && !lines[i].trim().startsWith("```")) {
        code.push(lines[i]);
        i += 1;
      }
      if (i < lines.length) {
        i += 1;
      }
      out.push(
        `<pre class="md-pre"><code${lang ? ` data-lang="${escapeHTML(lang)}"` : ""}>${escapeHTML(code.join("\n"))}</code></pre>`,
      );
      continue;
    }

    const heading = line.match(/^(#{1,6})\s+(.*)$/);
    if (heading) {
      const level = heading[1].length;
      out.push(`<h${level}>${renderInlineMarkdown(heading[2].trim())}</h${level}>`);
      i += 1;
      continue;
    }

    if (/^>\s?/.test(trimmed)) {
      const quote = [];
      while (i < lines.length && /^>\s?/.test(lines[i].trim())) {
        quote.push(lines[i].trim().replace(/^>\s?/, ""));
        i += 1;
      }
      out.push(`<blockquote>${quote.map((part) => renderInlineMarkdown(part)).join("<br>")}</blockquote>`);
      continue;
    }

    if (/^[-*]\s+/.test(trimmed)) {
      const items = [];
      while (i < lines.length && /^[-*]\s+/.test(lines[i].trim())) {
        items.push(lines[i].trim().replace(/^[-*]\s+/, ""));
        i += 1;
      }
      out.push(`<ul>${items.map((item) => `<li>${renderInlineMarkdown(item)}</li>`).join("")}</ul>`);
      continue;
    }

    if (/^\d+\.\s+/.test(trimmed)) {
      const items = [];
      while (i < lines.length && /^\d+\.\s+/.test(lines[i].trim())) {
        items.push(lines[i].trim().replace(/^\d+\.\s+/, ""));
        i += 1;
      }
      out.push(`<ol>${items.map((item) => `<li>${renderInlineMarkdown(item)}</li>`).join("")}</ol>`);
      continue;
    }

    const para = [];
    while (i < lines.length) {
      const next = lines[i];
      const nextTrimmed = next.trim();
      if (
        !nextTrimmed ||
        nextTrimmed.startsWith("```") ||
        /^#{1,6}\s+/.test(next) ||
        /^>\s?/.test(nextTrimmed) ||
        /^[-*]\s+/.test(nextTrimmed) ||
        /^\d+\.\s+/.test(nextTrimmed)
      ) {
        break;
      }
      para.push(nextTrimmed);
      i += 1;
    }
    out.push(`<p>${renderInlineMarkdown(para.join(" "))}</p>`);
  }

  return out.join("");
}

function formatBytes(value) {
  const size = Number(value || 0);
  if (!Number.isFinite(size) || size <= 0) {
    return "0 B";
  }
  const units = ["B", "KB", "MB", "GB"];
  let idx = 0;
  let current = size;
  while (current >= 1024 && idx < units.length - 1) {
    current /= 1024;
    idx += 1;
  }
  return `${current.toFixed(current >= 10 || idx === 0 ? 0 : 1)} ${units[idx]}`;
}

function formatUSD(cents) {
  const amount = Number(cents || 0) / 100;
  return new Intl.NumberFormat("en-US", {
    style: "currency",
    currency: "USD",
  }).format(amount);
}

function balanceStatus(state) {
  const status = String(state?.props?.currentBalanceStatus || "").trim();
  if (status === "ready" || status === "loading" || status === "unavailable") {
    return status;
  }
  return Number.isFinite(Number(state?.props?.currentBalanceCents)) ? "ready" : "loading";
}

function balanceCents(state) {
  const cents = Number(state?.props?.currentBalanceCents);
  return Number.isFinite(cents) ? cents : 0;
}

function balanceLabel(state) {
  switch (balanceStatus(state)) {
    case "ready":
      return formatUSD(balanceCents(state));
    case "unavailable":
      return "Unavailable";
    default:
      return "Checking...";
  }
}

function normalizeMessage(msg) {
  return {
    id: String(msg?.id || `local_${Math.random().toString(36).slice(2)}`),
    role: msg?.role === "assistant" ? "assistant" : "user",
    text: String(msg?.text || ""),
    keySource: String(msg?.keySource || ""),
    createdAt: String(msg?.createdAt || ""),
    attachments: Array.isArray(msg?.attachments) ? msg.attachments : [],
    toolTrace: Array.isArray(msg?.toolTrace) ? msg.toolTrace : [],
    pending: !!msg?.pending,
    error: !!msg?.error,
  };
}

function hasConversationPayload(props) {
  return (
    !!props &&
    typeof props === "object" &&
    String(props.conversationId || "").trim() !== "" &&
    Array.isArray(props.messages) &&
    Array.isArray(props.modelOptions)
  );
}

function byokStorageKey(provider) {
  return `vai-lite:byok:${provider}`;
}

function preferredKeySourceStorageKey(conversationId) {
  return `vai-lite:key-source:${conversationId}`;
}

function loadBYOK(providerHints) {
  const out = {};
  for (const hint of providerHints || []) {
    const provider = String(hint?.provider || "").trim();
    if (!provider) {
      continue;
    }
    try {
      out[provider] = localStorage.getItem(byokStorageKey(provider)) || "";
    } catch {
      out[provider] = "";
    }
  }
  return out;
}

function saveBYOK(provider, value) {
  try {
    localStorage.setItem(byokStorageKey(provider), value);
  } catch {}
}

function preferredKeySource(props) {
  try {
    const stored = localStorage.getItem(preferredKeySourceStorageKey(props.conversationId));
    if (
      stored === "platform_hosted" ||
      stored === "customer_byok_browser" ||
      stored === "customer_byok_vault"
    ) {
      return stored;
    }
  } catch {}
  if (
    props.initialKeySource === "platform_hosted" ||
    props.initialKeySource === "customer_byok_browser" ||
    props.initialKeySource === "customer_byok_vault"
  ) {
    return props.initialKeySource;
  }
  if (props.platformHostedEnabled) {
    return "platform_hosted";
  }
  if (props.hasWorkspaceProviders) {
    return "customer_byok_vault";
  }
  return "customer_byok_browser";
}

function persistPreferredKeySource(conversationId, value) {
  try {
    localStorage.setItem(preferredKeySourceStorageKey(conversationId), value);
  } catch {}
}

function createElement(html) {
  const template = document.createElement("template");
  template.innerHTML = html.trim();
  return template.content.firstElementChild;
}

function errorMessageFromValue(value, fallback = "Request failed") {
  if (!value) {
    return fallback;
  }
  if (typeof value === "string") {
    return value;
  }
  if (typeof value?.message === "string" && value.message.trim()) {
    return value.message;
  }
  if (typeof value?.error === "string" && value.error.trim()) {
    return value.error;
  }
  if (value?.error) {
    return errorMessageFromValue(value.error, fallback);
  }
  if (typeof value?.detail === "string" && value.detail.trim()) {
    return value.detail;
  }
  return fallback;
}

function canUseBrowserBYOK(state) {
  return !!state.props.allowBrowserBYOK;
}

function canUseWorkspaceBYOK(state) {
  return !!state.props.hasWorkspaceProviders;
}

function canUsePlatformHosted(state) {
  return !!state.props.platformHostedEnabled;
}

function hostedModelAvailable(state) {
  return Array.isArray(state.props.hostedModels)
    ? state.props.hostedModels.some((item) => String(item?.id || "") === String(state.currentModel || ""))
    : false;
}

function keySourceLabel(source) {
  switch (String(source || "")) {
    case "platform_hosted":
      return "VAI credits";
    case "customer_byok_vault":
      return "Workspace key";
    case "customer_byok_browser":
      return "Browser key";
    case "customer_byok_external":
      return "External BYOK";
    default:
      return source || "unknown";
  }
}

function effectiveKeySource(state) {
  if (state.keySource === "customer_byok_browser" && canUseBrowserBYOK(state)) {
    return "customer_byok_browser";
  }
  if (state.keySource === "customer_byok_vault" && canUseWorkspaceBYOK(state)) {
    return "customer_byok_vault";
  }
  if (state.keySource === "platform_hosted" && canUsePlatformHosted(state)) {
    return "platform_hosted";
  }
  if (canUsePlatformHosted(state)) {
    return "platform_hosted";
  }
  if (canUseWorkspaceBYOK(state)) {
    return "customer_byok_vault";
  }
  return "customer_byok_browser";
}

function byokHeaders(state) {
  const headers = {};
  for (const hint of state.props.providerHints || []) {
    const provider = String(hint?.provider || "").trim();
    const header = String(hint?.header || "").trim();
    if (!provider || !header) {
      continue;
    }
    const value = String(state.byok[provider] || "").trim();
    if (value) {
      headers[header] = value;
    }
  }
  return headers;
}

function hasAnyBYOK(state) {
  return Object.values(byokHeaders(state)).some((value) => String(value || "").trim() !== "");
}

function buildEventSourceParser(onEvent) {
  const decoder = new TextDecoder();
  let buffer = "";

  function flushChunk(chunk) {
    const parts = chunk.split(/\n\n/);
    buffer = parts.pop() || "";
    for (const part of parts) {
      const lines = part.split("\n");
      let eventName = "";
      const dataLines = [];
      for (const rawLine of lines) {
        const line = rawLine.trimEnd();
        if (line.startsWith("event:")) {
          eventName = line.slice(6).trim();
        } else if (line.startsWith("data:")) {
          dataLines.push(line.slice(5).trim());
        }
      }
      if (!eventName) {
        continue;
      }
      let data = null;
      const raw = dataLines.join("\n");
      if (raw) {
        try {
          data = JSON.parse(raw);
        } catch {
          data = raw;
        }
      }
      onEvent({ event: eventName, data });
    }
  }

  return {
    push(chunk) {
      buffer += decoder.decode(chunk, { stream: true }).replace(/\r\n/g, "\n");
      flushChunk(buffer);
    },
    finish() {
      if (buffer.trim()) {
        flushChunk(`${buffer}\n\n`);
      }
      decoder.decode();
    },
  };
}

function renderToolTrace(trace) {
  if (!Array.isArray(trace) || trace.length === 0) {
    return "";
  }
  return `
    <details class="tool-trace">
      <summary>Tool trace (${trace.length})</summary>
      <div class="tool-trace-list">
        ${trace
          .map((step, idx) => {
            const label = step?.name || step?.tool_name || step?.type || `step ${idx + 1}`;
            return `
              <article class="tool-trace-item">
                <header>${escapeHTML(label)}</header>
                <pre>${escapeHTML(JSON.stringify(step, null, 2))}</pre>
              </article>
            `;
          })
          .join("")}
      </div>
    </details>
  `;
}

function attachmentMarkup(attachment) {
  const name = escapeHTML(attachment.filename || "Attachment");
  const url = sanitizeURL(attachment.url || "#");
  const contentType = String(attachment.contentType || "");
  const isImage = contentType.startsWith("image/");
  return `
    <figure class="message-attachment${isImage ? " message-attachment-image" : ""}">
      ${
        isImage && attachment.url
          ? `<img src="${escapeHTML(url)}" alt="${name}" loading="lazy">`
          : `<div class="attachment-fallback">${name.slice(0, 1).toUpperCase()}</div>`
      }
      <figcaption>
        <a href="${escapeHTML(url)}" target="_blank" rel="noreferrer">${name}</a>
        <span>${escapeHTML(formatBytes(attachment.sizeBytes))}</span>
      </figcaption>
    </figure>
  `;
}

function renderMessagesHTML(messages, streaming) {
  if (!messages.length) {
    return `
      <section class="chat-empty">
        <h2>Start the conversation</h2>
        <p>Choose VAI credits, a workspace key, or browser-local BYOK. Responses stream live and render markdown in this island.</p>
      </section>
    `;
  }

  const lastUserIndex = messages.map((m) => m.role).lastIndexOf("user");
  const lastAssistantIndex = messages.map((m) => m.role).lastIndexOf("assistant");

  return messages
    .map((message, idx) => {
      const isLastAssistant = message.role === "assistant" && idx === lastAssistantIndex && !message.pending;
      const textHTML =
        message.role === "assistant"
          ? renderMarkdown(message.text)
          : `<p>${String(message.text || "")
              .split("\n")
              .map((line) => escapeHTML(line))
              .join("<br>")}</p>`;

      return `
        <article class="message message-${escapeHTML(message.role)}${message.pending ? " message-pending" : ""}${
          message.error ? " message-error" : ""
        }">
          <header class="message-meta">
            <span class="message-role">${message.role === "assistant" ? "Assistant" : "You"}</span>
            <span class="message-source">${escapeHTML(keySourceLabel(message.keySource || ""))}</span>
            ${
              message.createdAt
                ? `<time datetime="${escapeHTML(message.createdAt)}">${escapeHTML(
                    new Date(message.createdAt).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" }),
                  )}</time>`
                : ""
            }
          </header>
          <div class="message-body markdown-body">${textHTML || (message.pending ? "<p>Thinking…</p>" : "<p>(no text content)</p>")}</div>
          ${
            Array.isArray(message.attachments) && message.attachments.length
              ? `<div class="message-attachments">${message.attachments.map(attachmentMarkup).join("")}</div>`
              : ""
          }
          ${renderToolTrace(message.toolTrace)}
          <footer class="message-actions">
            ${
              message.role === "user"
                ? `<button type="button" class="ghost-action" data-action="edit" data-message-id="${escapeHTML(message.id)}">Edit</button>`
                : ""
            }
            ${
              isLastAssistant && !streaming && lastUserIndex >= 0
                ? `<button type="button" class="ghost-action" data-action="regenerate">Regenerate</button>`
                : ""
            }
          </footer>
        </article>
      `;
    })
    .join("");
}

function topPendingAttachmentHTML(attachment, idx) {
  return `
    <article class="pending-attachment">
      <div>
        <strong>${escapeHTML(attachment.filename)}</strong>
        <span>${escapeHTML(formatBytes(attachment.sizeBytes))}</span>
      </div>
      <button type="button" class="ghost-action" data-action="remove-attachment" data-index="${idx}">Remove</button>
    </article>
  `;
}

export function mount(el, props, api) {
  const pendingRequests = new Map();
  const state = {
    props: props || {},
    messages: Array.isArray(props?.messages) ? props.messages.map(normalizeMessage) : [],
    pendingAttachments: [],
    byok: loadBYOK(props?.providerHints),
    keySource: preferredKeySource(props || {}),
    currentModel:
      props?.model ||
      (Array.isArray(props?.modelOptions) ? props.modelOptions.find((item) => item?.selected)?.id : "") ||
      "oai-resp/gpt-5-mini",
    draft: "",
    editMessageId: "",
    busy: false,
    status: "",
    statusTone: "neutral",
    activeRunRequestId: "",
    activeAssistantMessage: null,
    requestSeq: 0,
  };

  const root = createElement(`
    <div class="chat-island-shell">
      <section class="chat-control-bar">
        <div class="control-group">
          <label class="control-label" for="chat-model">Model</label>
          <select id="chat-model" class="control-input"></select>
        </div>
        <div class="control-group control-group-wide">
          <div class="control-label">Key source</div>
          <div class="segmented" data-role="key-source"></div>
        </div>
          <div class="control-group control-group-meta">
          <div class="balance-chip">VAI credits <strong data-role="balance"></strong></div>
          <a class="ghost-link" data-role="settings-keys" href="#">Workspace keys</a>
          <a class="ghost-link" data-role="settings-billing" href="#">Billing</a>
        </div>
      </section>
      <section class="chat-banner" data-role="banner" hidden></section>
      <section class="byok-panel" data-role="byok-panel" hidden></section>
      <section class="message-list" data-role="messages"></section>
      <section class="pending-attachments-wrap">
        <div class="pending-attachments" data-role="pending-attachments"></div>
      </section>
      <form class="composer" data-role="composer">
        <div class="composer-meta" data-role="composer-meta"></div>
        <textarea class="composer-input" rows="4" placeholder="Send a message. Shift+Enter for a newline." data-role="composer-input"></textarea>
        <div class="composer-actions">
          <label class="btn btn-secondary btn-file">
            <input type="file" accept="image/*" multiple data-role="file-input" hidden>
            <span>Add images</span>
          </label>
          <div class="composer-spacer"></div>
          <button type="button" class="btn btn-secondary" data-role="stop-button" hidden>Stop</button>
          <button type="submit" class="btn btn-primary" data-role="send-button">Send</button>
        </div>
      </form>
    </div>
  `);

  el.innerHTML = "";
  el.appendChild(root);

  const refs = {
    banner: root.querySelector('[data-role="banner"]'),
    modelSelect: root.querySelector("#chat-model"),
    keySource: root.querySelector('[data-role="key-source"]'),
    balance: root.querySelector('[data-role="balance"]'),
    byokPanel: root.querySelector('[data-role="byok-panel"]'),
    messages: root.querySelector('[data-role="messages"]'),
    pendingAttachments: root.querySelector('[data-role="pending-attachments"]'),
    composer: root.querySelector('[data-role="composer"]'),
    composerMeta: root.querySelector('[data-role="composer-meta"]'),
    textarea: root.querySelector('[data-role="composer-input"]'),
    fileInput: root.querySelector('[data-role="file-input"]'),
    sendButton: root.querySelector('[data-role="send-button"]'),
    stopButton: root.querySelector('[data-role="stop-button"]'),
    settingsKeys: root.querySelector('[data-role="settings-keys"]'),
    settingsBilling: root.querySelector('[data-role="settings-billing"]'),
  };

  refs.settingsKeys.href = String(state.props.settingsKeysURL || "/settings/keys");
  refs.settingsBilling.href = String(state.props.settingsBillingURL || "/settings/billing");

  function nextRequestId(prefix) {
    state.requestSeq += 1;
    return `${String(prefix || "req")}_${Date.now()}_${state.requestSeq}`;
  }

  function startRequest(type, payload) {
    const requestId = nextRequestId(type);
    const message = { type, requestId, ...payload };
    const promise = new Promise((resolve, reject) => {
      pendingRequests.set(requestId, { resolve, reject });
    });
    try {
      api.send(message);
    } catch (err) {
      pendingRequests.delete(requestId);
      throw err;
    }
    return { requestId, promise };
  }

  function resolveRequest(requestId, value) {
    const pending = pendingRequests.get(requestId);
    if (!pending) {
      return;
    }
    pendingRequests.delete(requestId);
    pending.resolve(value);
  }

  function rejectRequest(requestId, message) {
    const pending = pendingRequests.get(requestId);
    if (!pending) {
      return;
    }
    pendingRequests.delete(requestId);
    pending.reject(new Error(message || "Request failed"));
  }

  function activeAssistantFor(requestId) {
    if (!requestId || state.activeRunRequestId !== requestId) {
      return null;
    }
    return state.activeAssistantMessage;
  }

  function clearActiveRun(requestId) {
    if (!requestId || state.activeRunRequestId !== requestId) {
      return;
    }
    state.activeRunRequestId = "";
    state.activeAssistantMessage = null;
  }

  function resetComposerAfterAcceptedRun() {
    state.pendingAttachments = [];
    state.draft = "";
    state.editMessageId = "";
    refs.textarea.value = "";
    renderPendingAttachments();
    renderComposerMeta();
  }

  function setStatus(message, tone = "neutral") {
    state.status = String(message || "");
    state.statusTone = tone;
    renderBanner();
  }

  function renderBanner() {
    if (!state.status) {
      refs.banner.hidden = true;
      refs.banner.className = "chat-banner";
      refs.banner.textContent = "";
      return;
    }
    refs.banner.hidden = false;
    refs.banner.className = `chat-banner chat-banner-${state.statusTone}`;
    refs.banner.textContent = state.status;
  }

  function renderModelOptions() {
    const options = Array.isArray(state.props.modelOptions) ? state.props.modelOptions : [];
    refs.modelSelect.innerHTML = options
      .map((item) => {
        const id = String(item?.id || "");
        const label = String(item?.label || id);
        const selected = id === state.currentModel ? ' selected="selected"' : "";
        return `<option value="${escapeHTML(id)}"${selected}>${escapeHTML(label)}</option>`;
      })
      .join("");
  }

  function renderKeySourceControls() {
    const current = effectiveKeySource(state);
    const canBrowser = canUseBrowserBYOK(state);
    refs.keySource.innerHTML = `
      <button type="button" class="segment${current === "platform_hosted" ? " segment-active" : ""}" data-key-source="platform_hosted"${
        canUsePlatformHosted(state) ? "" : ' disabled="disabled"'
      }>Use VAI credits</button>
      <button type="button" class="segment${current === "customer_byok_vault" ? " segment-active" : ""}" data-key-source="customer_byok_vault"${
        canUseWorkspaceBYOK(state) ? "" : ' disabled="disabled"'
      }>Use workspace key</button>
      <button type="button" class="segment${current === "customer_byok_browser" ? " segment-active" : ""}" data-key-source="customer_byok_browser"${
        canBrowser ? "" : ' disabled="disabled"'
      }>Use browser key</button>
    `;
  }

  function renderBYOKPanel() {
    const show = effectiveKeySource(state) === "customer_byok_browser";
    refs.byokPanel.hidden = !show;
    if (!show) {
      refs.byokPanel.innerHTML = "";
      return;
    }
    refs.byokPanel.innerHTML = `
      <header class="panel-header">
        <div>
          <h3>Browser-local provider keys</h3>
          <p>Keys stay in this browser only. They are sent only with the active chat request and are never stored server-side.</p>
        </div>
      </header>
      <div class="byok-grid">
        ${(state.props.providerHints || [])
          .map((hint) => {
            const provider = String(hint?.provider || "");
            return `
              <label class="byok-field">
                <span>${escapeHTML(provider)}</span>
                <input type="password" data-provider-input="${escapeHTML(provider)}" value="${escapeHTML(state.byok[provider] || "")}" placeholder="Paste ${escapeHTML(provider)} key">
              </label>
            `;
          })
          .join("")}
      </div>
    `;
  }

  function renderPendingAttachments() {
    refs.pendingAttachments.innerHTML = state.pendingAttachments.map(topPendingAttachmentHTML).join("");
  }

  function renderComposerMeta() {
    const source = effectiveKeySource(state);
    const hostedBalanceStatus = balanceStatus(state);
    const chips = [];
    chips.push(`<span class="meta-chip">${escapeHTML(keySourceLabel(source))}</span>`);
    chips.push(`<span class="meta-chip">Model ${escapeHTML(state.currentModel)}</span>`);
    if (state.editMessageId) {
      chips.push(
        `<button type="button" class="ghost-action" data-action="cancel-edit">Cancel edit</button><span class="meta-chip meta-chip-warning">Editing earlier message</span>`,
      );
    }
    if (source === "platform_hosted" && !hostedModelAvailable(state)) {
      chips.push(`<span class="meta-chip meta-chip-danger">Selected model is not available in VAI-hosted mode</span>`);
    }
    if (source === "platform_hosted" && hostedBalanceStatus === "loading") {
      chips.push(`<span class="meta-chip meta-chip-warning">Checking VAI credits</span>`);
    }
    if (source === "platform_hosted" && hostedBalanceStatus === "unavailable") {
      chips.push(`<span class="meta-chip meta-chip-warning">VAI credit balance unavailable</span>`);
    }
    if (source === "platform_hosted" && hostedBalanceStatus === "ready" && balanceCents(state) <= 0) {
      chips.push(`<span class="meta-chip meta-chip-danger">VAI credits depleted</span>`);
    }
    if (source === "customer_byok_vault" && !canUseWorkspaceBYOK(state)) {
      chips.push(`<span class="meta-chip meta-chip-warning">No workspace key stored</span>`);
    }
    refs.composerMeta.innerHTML = chips.join("");
  }

  function scrollMessagesToEnd() {
    requestAnimationFrame(() => {
      refs.messages.scrollTop = refs.messages.scrollHeight;
    });
  }

  function renderMessages() {
    refs.messages.innerHTML = renderMessagesHTML(state.messages, state.busy);
    scrollMessagesToEnd();
  }

  function updateBusyControls() {
    refs.sendButton.disabled = state.busy;
    refs.modelSelect.disabled = state.busy;
    refs.fileInput.disabled = state.busy;
    refs.stopButton.hidden = !state.busy;
    refs.stopButton.disabled = !state.busy;
    refs.sendButton.textContent = state.editMessageId ? "Save and rerun" : "Send";
  }

  function renderStaticBits() {
    refs.balance.textContent = balanceLabel(state);
    renderModelOptions();
    renderKeySourceControls();
    renderBYOKPanel();
    renderPendingAttachments();
    renderComposerMeta();
    renderMessages();
    renderBanner();
    updateBusyControls();
  }

  function browserKeyPayload() {
    const keys = {};
    for (const [provider, value] of Object.entries(state.byok || {})) {
      const trimmed = String(value || "").trim();
      if (trimmed) {
        keys[provider] = trimmed;
      }
    }
    return keys;
  }

  async function uploadFile(file) {
    const contentType = file.type || "application/octet-stream";
    const { promise: intentPromise } = startRequest("upload_intent", {
      filename: file.name,
      contentType,
      sizeBytes: file.size,
    });
    const intentResponse = await intentPromise;
    const intent = intentResponse?.intent;
    if (!intent?.upload_url || !intent?.intent_token) {
      throw new Error("Upload intent failed");
    }

    const putHeaders = new Headers(intent.headers || {});
    putHeaders.set("Content-Type", intent.content_type || contentType);
    const putResp = await fetch(intent.upload_url, {
      method: "PUT",
      headers: putHeaders,
      body: file,
    });
    if (!putResp.ok) {
      throw new Error(`Upload failed (${putResp.status})`);
    }

    const { promise: claimPromise } = startRequest("upload_claim", {
      filename: file.name,
      contentType,
      sizeBytes: file.size,
      intentToken: intent.intent_token,
    });
    const claimResponse = await claimPromise;
    if (!claimResponse?.attachment) {
      throw new Error("Upload claim failed");
    }
    return claimResponse.attachment;
  }

  async function handleFiles(files) {
    const selected = Array.from(files || []);
    if (!selected.length) {
      return;
    }
    setStatus("Uploading attachments…", "neutral");
    try {
      for (const file of selected) {
        const attachment = await uploadFile(file);
        state.pendingAttachments.push(attachment);
        renderPendingAttachments();
      }
      setStatus(`Uploaded ${selected.length} attachment${selected.length === 1 ? "" : "s"}.`, "success");
    } catch (err) {
      setStatus(err?.message || "Attachment upload failed.", "error");
    } finally {
      refs.fileInput.value = "";
    }
  }

  function trimMessagesAfter(messageId) {
    const index = state.messages.findIndex((message) => message.id === messageId);
    if (index >= 0) {
      state.messages = state.messages.slice(0, index + 1);
    }
  }

  function lastUserMessage() {
    for (let i = state.messages.length - 1; i >= 0; i -= 1) {
      if (state.messages[i].role === "user") {
        return state.messages[i];
      }
    }
    return null;
  }

  function handleServerMessage(message) {
    const type = String(message?.type || "");
    const requestId = String(message?.requestId || "");
    const assistantMessage = activeAssistantFor(requestId);

    switch (type) {
      case "upload_intent_ready":
      case "upload_claim_ready":
        resolveRequest(requestId, message);
        return;
      case "upload_intent_error":
      case "upload_claim_error":
        rejectRequest(requestId, message?.error || "Upload request failed");
        return;
      case "chat_started":
        if (requestId && state.activeRunRequestId === requestId) {
          resetComposerAfterAcceptedRun();
        }
        return;
      case "chat_delta":
        if (!assistantMessage) {
          return;
        }
        assistantMessage.text += String(message?.delta || "");
        renderMessages();
        return;
      case "chat_tool":
        if (!assistantMessage) {
          return;
        }
        if (!Array.isArray(assistantMessage.toolTrace)) {
          assistantMessage.toolTrace = [];
        }
        assistantMessage.toolTrace.push(message?.tool);
        renderMessages();
        return;
      case "chat_complete":
        if (assistantMessage) {
          assistantMessage.pending = false;
          assistantMessage.error = false;
          if (typeof message?.assistant?.text === "string" && message.assistant.text.trim()) {
            assistantMessage.text = message.assistant.text;
          }
          if (message?.assistant?.toolTrace !== undefined) {
            assistantMessage.toolTrace = Array.isArray(message.assistant.toolTrace) ? message.assistant.toolTrace : message.assistant.toolTrace || [];
          }
        }
        clearActiveRun(requestId);
        renderMessages();
        resolveRequest(requestId, { type: "chat_complete", assistant: message?.assistant || null });
        return;
      case "chat_stopped":
        if (assistantMessage) {
          assistantMessage.pending = false;
          assistantMessage.error = false;
        }
        clearActiveRun(requestId);
        renderMessages();
        resolveRequest(requestId, { type: "chat_stopped" });
        return;
      case "chat_error":
        if (assistantMessage) {
          assistantMessage.pending = false;
          assistantMessage.error = true;
          assistantMessage.text = assistantMessage.text || `Stream failed: ${message?.error || "unknown error"}`;
        }
        clearActiveRun(requestId);
        renderMessages();
        rejectRequest(requestId, message?.error || "Streaming request failed.");
        return;
      default:
        return;
    }
  }

  async function submitComposer({ regenerate = false } = {}) {
    if (state.busy) {
      return;
    }

    const source = effectiveKeySource(state);
    const hostedBalanceStatus = balanceStatus(state);
    if (source === "customer_byok_browser" && !hasAnyBYOK(state)) {
      setStatus("Add at least one browser-local provider key before sending with BYOK.", "error");
      return;
    }
    if (source === "customer_byok_vault" && !canUseWorkspaceBYOK(state)) {
      setStatus("Store a workspace provider key before using workspace-key mode.", "error");
      return;
    }
    if (source === "platform_hosted" && !hostedModelAvailable(state)) {
      setStatus("The selected model is not available in VAI-hosted mode. Switch modes or choose a hosted model.", "error");
      return;
    }
    if (source === "platform_hosted" && hostedBalanceStatus === "ready" && balanceCents(state) <= 0) {
      setStatus("VAI credits are depleted. Add credits or switch to workspace/browser BYOK.", "error");
      return;
    }

    const text = state.draft.trim();
    if (!regenerate && !state.editMessageId && !text && state.pendingAttachments.length === 0) {
      setStatus("Type a message or attach an image before sending.", "error");
      return;
    }

    state.busy = true;
    updateBusyControls();
    setStatus(
      source === "platform_hosted"
        ? "Streaming response with VAI-hosted access…"
        : source === "customer_byok_vault"
          ? "Streaming response with workspace BYOK…"
          : "Streaming response with browser-local BYOK…",
      "neutral",
    );

    const assistantMessage = normalizeMessage({
      id: `pending_${Date.now()}`,
      role: "assistant",
      keySource: source,
      text: "",
      toolTrace: [],
      pending: true,
      createdAt: new Date().toISOString(),
    });

    if (state.editMessageId) {
      const existing = state.messages.find((message) => message.id === state.editMessageId);
      if (existing) {
        existing.text = text;
        trimMessagesAfter(state.editMessageId);
      }
    } else if (regenerate) {
      const lastUser = lastUserMessage();
      if (!lastUser) {
        setStatus("There is no user message to regenerate from yet.", "error");
        state.busy = false;
        updateBusyControls();
        return;
      }
      trimMessagesAfter(lastUser.id);
    } else {
      state.messages.push(
        normalizeMessage({
          id: `local_user_${Date.now()}`,
          role: "user",
          text,
          keySource: source,
          attachments: [...state.pendingAttachments],
          createdAt: new Date().toISOString(),
        }),
      );
    }

    state.messages.push(assistantMessage);
    renderMessages();

    try {
      const { requestId, promise } = startRequest("submit", {
        message: text,
        model: state.currentModel,
        keySource: source,
        attachmentIds: state.pendingAttachments.map((attachment) => attachment.id),
        regenerate,
        editMessageId: state.editMessageId || "",
        browserKeys: source === "customer_byok_browser" ? browserKeyPayload() : {},
      });
      state.activeRunRequestId = requestId;
      state.activeAssistantMessage = assistantMessage;

      const outcome = await promise;
      assistantMessage.pending = false;
      assistantMessage.error = false;
      if (typeof outcome?.assistant?.text === "string" && outcome.assistant.text.trim()) {
        assistantMessage.text = outcome.assistant.text;
      }
      if (outcome?.assistant?.toolTrace !== undefined) {
        assistantMessage.toolTrace = Array.isArray(outcome.assistant.toolTrace) ? outcome.assistant.toolTrace : outcome.assistant.toolTrace || [];
      }
      if (outcome?.type === "chat_stopped") {
        setStatus("Stopped streaming.", "warning");
      } else {
        setStatus("Response complete.", "success");
      }
      renderComposerMeta();
      renderMessages();
    } catch (err) {
      assistantMessage.pending = false;
      assistantMessage.error = true;
      assistantMessage.text = assistantMessage.text || `Stream failed: ${err?.message || "unknown error"}`;
      setStatus(err?.message || "Streaming request failed.", "error");
      renderMessages();
    } finally {
      state.busy = false;
      updateBusyControls();
      renderComposerMeta();
    }
  }

  refs.modelSelect.addEventListener("change", (event) => {
    state.currentModel = event.target.value;
    renderComposerMeta();
  });

  refs.keySource.addEventListener("click", (event) => {
    const button = event.target.closest("[data-key-source]");
    if (!button || button.disabled) {
      return;
    }
    state.keySource = button.dataset.keySource || "platform_hosted";
    persistPreferredKeySource(state.props.conversationId, state.keySource);
    renderKeySourceControls();
    renderBYOKPanel();
    renderComposerMeta();
  });

  refs.byokPanel.addEventListener("input", (event) => {
    const input = event.target.closest("[data-provider-input]");
    if (!input) {
      return;
    }
    const provider = input.getAttribute("data-provider-input");
    state.byok[provider] = input.value;
    saveBYOK(provider, input.value);
  });

  refs.fileInput.addEventListener("change", (event) => {
    handleFiles(event.target.files).catch((err) => {
      setStatus(err?.message || "Attachment upload failed.", "error");
    });
  });

  refs.pendingAttachments.addEventListener("click", (event) => {
    const button = event.target.closest('[data-action="remove-attachment"]');
    if (!button) {
      return;
    }
    const idx = Number(button.getAttribute("data-index"));
    if (!Number.isInteger(idx) || idx < 0 || idx >= state.pendingAttachments.length) {
      return;
    }
    state.pendingAttachments.splice(idx, 1);
    renderPendingAttachments();
  });

  refs.messages.addEventListener("click", (event) => {
    const button = event.target.closest("[data-action]");
    if (!button) {
      return;
    }
    const action = button.getAttribute("data-action");
    if (action === "edit") {
      const messageID = button.getAttribute("data-message-id");
      const message = state.messages.find((item) => item.id === messageID);
      if (!message || message.role !== "user") {
        return;
      }
      state.editMessageId = messageID;
      state.draft = message.text;
      refs.textarea.value = message.text;
      refs.textarea.focus();
      renderComposerMeta();
      setStatus("Editing earlier user message. Sending will rewrite the conversation tail.", "neutral");
      return;
    }
    if (action === "regenerate") {
      submitComposer({ regenerate: true }).catch((err) => {
        setStatus(err?.message || "Regeneration failed.", "error");
      });
    }
  });

  refs.composerMeta.addEventListener("click", (event) => {
    const button = event.target.closest('[data-action="cancel-edit"]');
    if (!button) {
      return;
    }
    state.editMessageId = "";
    state.draft = "";
    refs.textarea.value = "";
    renderComposerMeta();
    setStatus("Edit canceled.", "neutral");
  });

  refs.textarea.addEventListener("input", (event) => {
    state.draft = event.target.value;
  });

  refs.textarea.addEventListener("keydown", (event) => {
    if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      submitComposer().catch((err) => {
        setStatus(err?.message || "Message send failed.", "error");
      });
    }
  });

  refs.composer.addEventListener("submit", (event) => {
    event.preventDefault();
    submitComposer().catch((err) => {
      setStatus(err?.message || "Message send failed.", "error");
    });
  });

  refs.stopButton.addEventListener("click", () => {
    if (!state.activeRunRequestId) {
      return;
    }
    try {
      api.send({
        type: "stop",
        requestId: state.activeRunRequestId,
      });
      setStatus("Stopping stream…", "warning");
    } catch (err) {
      setStatus(err?.message || "Unable to stop the current stream.", "error");
    }
  });

  renderStaticBits();

  return {
    update(nextProps) {
      const previousConversationId = String(state.props?.conversationId || "");
      const nextConversationId = String(nextProps?.conversationId || "");
      const nextHasPayload = hasConversationPayload(nextProps);

      if (!nextHasPayload && previousConversationId) {
        setStatus("Conversation state is syncing. Keeping the last ready view.", "neutral");
        return;
      }

      state.props = nextProps || {};
      const conversationChanged = nextConversationId !== previousConversationId;
      if (conversationChanged) {
        state.keySource = preferredKeySource(state.props || {});
        state.pendingAttachments = [];
        state.draft = "";
        state.editMessageId = "";
        state.busy = false;
        state.activeRunRequestId = "";
        state.activeAssistantMessage = null;
        refs.textarea.value = "";
      }
      state.currentModel = String(state.props?.model || state.currentModel);
      if (
        Array.isArray(state.props?.messages) &&
        (!state.activeRunRequestId || conversationChanged)
      ) {
        state.messages = state.props.messages.map(normalizeMessage);
      }
      refs.settingsKeys.href = String(state.props.settingsKeysURL || "/settings/keys");
      refs.settingsBilling.href = String(state.props.settingsBillingURL || "/settings/billing");
      renderStaticBits();
    },
    destroy() {
      for (const pending of pendingRequests.values()) {
        pending.reject(new Error("Chat island unmounted"));
      }
      pendingRequests.clear();
      el.innerHTML = "";
    },
    onMessage(message) {
      handleServerMessage(message);
    },
    onReconnect() {
      setStatus("Connection restored. Refresh the page if the conversation looks stale.", "neutral");
    },
  };
}
