const toggle = document.querySelector(".nav-toggle");
const navigation = document.getElementById("docs-navigation");
toggle.addEventListener("click", () => {
  const expanded = toggle.getAttribute("aria-expanded") === "true";
  toggle.setAttribute("aria-expanded", String(!expanded));
  navigation.classList.toggle("is-open", !expanded);
});

for (const button of document.querySelectorAll(".copy-code")) {
  button.addEventListener("click", async () => {
    const code = button.closest(".code-block").querySelector("pre code, pre");
    try {
      await navigator.clipboard.writeText(code.textContent);
      button.textContent = "Copied";
      button.setAttribute("aria-label", "Code copied");
    } catch {
      button.textContent = "Copy failed";
      button.setAttribute("aria-label", "Copy failed. Select the code and copy it manually.");
    }
    window.setTimeout(() => {
      button.textContent = "Copy";
      button.setAttribute("aria-label", "Copy code");
    }, 2500);
  });
}

const pickers = document.querySelectorAll("details.picker");
document.addEventListener("click", event => {
  for (const picker of pickers) {
    if (picker.open && !picker.contains(event.target)) picker.open = false;
  }
});
document.addEventListener("keydown", event => {
  if (event.key !== "Escape") return;
  for (const picker of pickers) {
    if (picker.open && picker.contains(event.target)) {
      picker.open = false;
      picker.querySelector("summary").focus();
    }
  }
});
for (const picker of pickers) {
  picker.addEventListener("toggle", () => {
    if (!picker.open) return;
    for (const other of pickers) {
      if (other !== picker) other.open = false;
    }
  });
}

const themePicker = document.querySelector(".theme-picker");
if (themePicker) {
  const rows = themePicker.querySelectorAll(".theme-row");
  const root = document.documentElement;
  const darkQuery = window.matchMedia("(prefers-color-scheme: dark)");
  const metaTheme = document.querySelector('meta[name="theme-color"]');
  const choice = () => root.dataset.themeChoice || "system";
  const paintMeta = () => {
    if (!metaTheme) return;
    const dark = choice() === "dark" || (choice() === "system" && darkQuery.matches);
    metaTheme.setAttribute("content", dark ? "#15171a" : "#ffffff");
  };
  const apply = selected => {
    root.dataset.themeChoice = selected;
    if (selected === "light" || selected === "dark") root.dataset.theme = selected;
    else delete root.dataset.theme;
    try {
      if (selected === "system") localStorage.removeItem("theme");
      else localStorage.setItem("theme", selected);
    } catch {}
    for (const row of rows) {
      const active = row.dataset.themeChoice === selected;
      row.classList.toggle("is-active", active);
      if (active) row.setAttribute("aria-current", "true");
      else row.removeAttribute("aria-current");
    }
    paintMeta();
  };
  themePicker.addEventListener("click", event => {
    const row = event.target.closest(".theme-row");
    if (row) {
      apply(row.dataset.themeChoice);
      themePicker.open = false;
      themePicker.querySelector("summary").focus();
    }
  });
  darkQuery.addEventListener("change", () => {
    if (choice() === "system") paintMeta();
  });
  apply(choice());
}

const searchForm = document.querySelector(".search-form");
if (searchForm) {
  const input = document.getElementById("search-query");
  const status = document.getElementById("search-status");
  const results = document.getElementById("search-results");
  let indexPromise;
  let searchSequence = 0;
  async function search() {
    const sequence = ++searchSequence;
    const query = input.value.trim();
    results.replaceChildren();
    if (!query) {
      status.textContent = "Enter a term to search the documentation.";
      return;
    }
    status.textContent = "Searching…";
    try {
      indexPromise ||= fetch(results.dataset.index).then(response => {
        if (!response.ok) throw new Error("Search index unavailable");
        return response.json();
      }).catch(error => { indexPromise = undefined; throw error; });
      const pages = await indexPromise;
      if (sequence !== searchSequence) return;
      const terms = query.toLocaleLowerCase().split(/\s+/);
      const matches = pages.filter(page => {
        const text = `${page.title} ${page.content}`.toLocaleLowerCase();
        return terms.every(term => text.includes(term));
      }).sort((a, b) => {
        const score = page => terms.filter(term => page.title.toLocaleLowerCase().includes(term)).length;
        return score(b) - score(a) || a.title.localeCompare(b.title);
      });
      status.textContent = matches.length ? `${matches.length} ${matches.length === 1 ? "page" : "pages"} found for “${query}”.` : `No pages found for “${query}”. Try a different term.`;
      for (const page of matches) {
        const item = document.createElement("li");
        const link = document.createElement("a");
        link.href = page.url;
        link.textContent = page.title;
        const snippet = document.createElement("p");
        const text = page.content.replace(/\s+/g, " ").trim();
        const positions = terms.map(term => text.toLocaleLowerCase().indexOf(term)).filter(position => position >= 0);
        const match = positions.length ? Math.min(...positions) : 0;
        const start = Math.max(0, match - 70);
        snippet.textContent = `${start ? "…" : ""}${text.slice(start, start + 220)}${text.length > start + 220 ? "…" : ""}`;
        item.append(link, snippet);
        results.append(item);
      }
    } catch {
      if (sequence === searchSequence) status.textContent = "Search is unavailable. Try again, or browse the navigation.";
    }
  }
  input.value = new URLSearchParams(location.search).get("q") || "";
  searchForm.addEventListener("submit", event => {
    event.preventDefault();
    const url = new URL(location.href);
    url.searchParams.set("q", input.value.trim());
    history.replaceState(null, "", url);
    search();
  });
  search();
}

const diagrams = document.querySelectorAll(".diagram-block");
if (diagrams.length) {
  async function renderDiagrams() {
    await document.fonts.ready;
    if (window.mermaid) {
      mermaid.initialize({
        startOnLoad: false,
        securityLevel: "strict",
        suppressErrorRendering: true,
        theme: "neutral",
        fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif',
      });
    }
    for (const [index, block] of [...diagrams].entries()) {
      const target = block.querySelector(".mermaid-diagram");
      const status = block.querySelector(".diagram-status");
      try {
        if (!window.mermaid) throw new Error("Renderer unavailable");
        const source = block.querySelector("pre code").textContent;
        const { svg } = await mermaid.render(`diagram-${index}`, source);
        target.innerHTML = svg;
        status.hidden = true;
      } catch {
        target.hidden = true;
        status.textContent = "Diagram could not be rendered. View the Mermaid source below.";
        block.querySelector("details").open = true;
      } finally {
        target.setAttribute("aria-busy", "false");
      }
    }
  }
  renderDiagrams();
}


const versionPicker = document.querySelector(".version-picker");
if (versionPicker) {
  const status = document.getElementById("version-status");
  const note = versionPicker.querySelector(".picker-note");
  const root = new URL(versionPicker.dataset.root);
  const currentPath = versionPicker.dataset.page.slice(root.pathname.length);
  // Keep the language and article path when another version contains that page.
  const articlePath = currentPath.slice(currentPath.indexOf("/") + 1);
  note.textContent = "Loading versions…";
  fetch(new URL("versions.json", root)).then(response => {
    if (!response.ok) throw new Error("Version index unavailable");
    return response.json();
  }).then(index => {
    if (!Array.isArray(index.versions)) throw new Error("Version index malformed");
    for (const version of index.versions) {
      if (version.name === versionPicker.dataset.version) continue;
      const row = document.createElement("button");
      row.type = "button";
      row.className = "picker-row version-row";
      row.textContent = version.name === "main" ? "main (development)" : version.name;
      row.dataset.home = new URL(version.path, root).href;
      row.dataset.target = new URL(articlePath, row.dataset.home).href;
      note.before(row);
    }
    note.remove();
    status.textContent = "Documentation versions loaded.";
  }).catch(() => {
    note.textContent = "Version list unavailable.";
    status.textContent = "Version list is unavailable. This documentation version is still readable.";
  });
  versionPicker.addEventListener("click", async event => {
    const row = event.target.closest(".version-row");
    if (!row) return;
    for (const other of versionPicker.querySelectorAll(".version-row")) other.disabled = true;
    status.textContent = "Opening documentation version…";
    try {
      const response = await fetch(row.dataset.target, { method: "HEAD" });
      window.location.assign(response.ok ? row.dataset.target : row.dataset.home);
    } catch {
      for (const other of versionPicker.querySelectorAll(".version-row")) other.disabled = false;
      status.textContent = "Could not open that version. Try again.";
    }
  });
}
