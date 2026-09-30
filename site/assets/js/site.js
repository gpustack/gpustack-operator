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


const versionPicker = document.getElementById("docs-version");
if (versionPicker) {
  const status = document.getElementById("version-status");
  const root = new URL(versionPicker.dataset.root);
  const currentPath = versionPicker.dataset.page.slice(root.pathname.length);
  // Keep the language and article path when another version contains that page.
  const articlePath = currentPath.slice(currentPath.indexOf("/") + 1);
  fetch(new URL("versions.json", root)).then(response => {
    if (!response.ok) throw new Error("Version index unavailable");
    return response.json();
  }).then(index => {
    for (const version of index.versions) {
      if (version.name === versionPicker.dataset.version) continue;
      const option = document.createElement("option");
      option.value = new URL(version.path, root).href;
      option.textContent = version.name === "main" ? "main (development)" : version.name;
      versionPicker.append(option);
    }
    versionPicker.disabled = false;
    status.textContent = "Documentation versions loaded.";
  }).catch(() => {
    status.textContent = "Version list is unavailable. This documentation version is still readable.";
  });
  versionPicker.addEventListener("change", async () => {
    const home = new URL(versionPicker.value, window.location.origin);
    const target = new URL(articlePath, home);
    versionPicker.disabled = true;
    status.textContent = "Opening documentation version…";
    try {
      const response = await fetch(target, { method: "HEAD" });
      window.location.assign(response.ok ? target.href : home.href);
    } catch {
      versionPicker.disabled = false;
      status.textContent = "Could not open that version. Try again.";
    }
  });
}
