const toggle = document.querySelector(".nav-toggle");
const navigation = document.getElementById("docs-navigation");
toggle.addEventListener("click", () => {
  const expanded = toggle.getAttribute("aria-expanded") === "true";
  toggle.setAttribute("aria-expanded", String(!expanded));
  navigation.classList.toggle("is-open", !expanded);
});

const sidebarToggle = document.querySelector(".sidebar-toggle");
if (sidebarToggle) {
  const root = document.documentElement;
  const layout = document.querySelector(".layout");
  const isCollapsed = () => root.dataset.sidebarCollapsed === "true";
  const syncToggle = collapsed => {
    sidebarToggle.setAttribute("aria-expanded", String(!collapsed));
    sidebarToggle.setAttribute("aria-label", collapsed ? "Expand sidebar" : "Collapse sidebar");
    sidebarToggle.setAttribute("title", collapsed ? "Expand sidebar" : "Collapse sidebar");
  };

  syncToggle(isCollapsed());

  sidebarToggle.addEventListener("click", () => {
    const next = !isCollapsed();
    if (next) {
      root.dataset.sidebarCollapsed = "true";
      try { localStorage.setItem("sidebar-collapsed", "true"); } catch {}
    } else {
      delete root.dataset.sidebarCollapsed;
      try { localStorage.removeItem("sidebar-collapsed"); } catch {}
    }
    syncToggle(next);
    const dispatchResize = () => window.dispatchEvent(new Event("resize"));
    if (layout && window.getComputedStyle(layout).transitionDuration !== "0s") {
      layout.addEventListener("transitionend", dispatchResize, { once: true });
    } else {
      dispatchResize();
    }
  });

  document.addEventListener("keydown", event => {
    if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "b") {
      // Skip when .sidebar-toggle is hidden by the mobile media query in site.css.
      if (window.getComputedStyle(sidebarToggle).display === "none") return;
      const active = document.activeElement;
      if (active && (active.tagName === "INPUT" || active.tagName === "TEXTAREA" || active.isContentEditable)) return;
      event.preventDefault();
      sidebarToggle.click();
    }
  });
}

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
    window.dispatchEvent(new Event("resize"));
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
  fetch(new URL("versions.json", root), { cache: "no-store" }).then(response => {
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

const tocPanel = document.querySelector(".toc-panel");
if (tocPanel) {
  const tocInner = tocPanel.querySelector(".toc-inner");
  const tocNav = tocPanel.querySelector("nav#TableOfContents, .toc-inner > nav:not(.page-actions)");
  if (tocInner && tocNav) {
    let indicator = tocNav.querySelector(".toc-indicator");
    if (!indicator) {
      indicator = document.createElement("div");
      indicator.className = "toc-indicator";
      indicator.setAttribute("aria-hidden", "true");
      tocNav.prepend(indicator);
    }

    const linkMap = new Map();
    const links = Array.from(tocNav.querySelectorAll('a[href^="#"]'));
    for (const link of links) {
      const href = link.getAttribute("href");
      if (!href || href === "#" || href === "#contents") continue;
      const id = decodeURIComponent(href.slice(1));
      linkMap.set(id, link);
    }

    const headings = Array.from(document.querySelectorAll("article h2[id], article h3[id]"))
      .filter(heading => heading.id !== "contents" && linkMap.has(heading.id));

    let currentActiveLink = null;

    const prefersReducedMotion = () => window.matchMedia("(prefers-reduced-motion: reduce)").matches;

    const updateIndicator = link => {
      if (!link) {
        indicator.style.opacity = "0";
        return;
      }
      const navRect = tocNav.getBoundingClientRect();
      const linkRect = link.getBoundingClientRect();
      const top = linkRect.top - navRect.top;
      const height = linkRect.height;
      indicator.style.transform = `translateY(${top}px)`;
      indicator.style.height = `${height}px`;
      indicator.style.opacity = "1";
    };

    const scrollTocIntoView = link => {
      if (!link) return;
      const innerRect = tocInner.getBoundingClientRect();
      const linkRect = link.getBoundingClientRect();
      const margin = 40;
      const reduced = prefersReducedMotion();

      if (linkRect.top < innerRect.top + margin) {
        const diff = linkRect.top - (innerRect.top + margin);
        tocInner.scrollBy({ top: diff, behavior: reduced ? "auto" : "smooth" });
      } else if (linkRect.bottom > innerRect.bottom - margin) {
        const diff = linkRect.bottom - (innerRect.bottom - margin);
        tocInner.scrollBy({ top: diff, behavior: reduced ? "auto" : "smooth" });
      }
    };

    const setActiveLink = (link, autoScrollToc = true) => {
      if (link === currentActiveLink) return;
      if (currentActiveLink) {
        delete currentActiveLink.dataset.active;
        currentActiveLink.classList.remove("is-active");
        currentActiveLink.removeAttribute("aria-current");
        currentActiveLink.closest("li")?.classList.remove("is-active");
      }
      currentActiveLink = link;
      if (link) {
        link.dataset.active = "true";
        link.classList.add("is-active");
        link.setAttribute("aria-current", "true");
        link.closest("li")?.classList.add("is-active");
        updateIndicator(link);
        if (autoScrollToc) scrollTocIntoView(link);
      } else {
        updateIndicator(null);
      }
    };

    const findActiveHeading = () => {
      if (!headings.length) return null;
      const scrollBottom = window.innerHeight + window.scrollY;
      const isAtBottom = scrollBottom >= document.documentElement.scrollHeight - 25;
      if (isAtBottom) {
        return headings[headings.length - 1];
      }
      const triggerOffset = 120;
      let active = null;
      for (const heading of headings) {
        const top = heading.getBoundingClientRect().top;
        if (top <= triggerOffset) {
          active = heading;
        } else {
          break;
        }
      }
      if (!active && headings.length > 0) {
        const firstTop = headings[0].getBoundingClientRect().top;
        if (firstTop < window.innerHeight * 0.5) {
          active = headings[0];
        }
      }
      return active;
    };

    const updateScrollSpy = (autoScrollToc = true) => {
      const activeHeading = findActiveHeading();
      if (activeHeading) {
        setActiveLink(linkMap.get(activeHeading.id), autoScrollToc);
      } else {
        setActiveLink(null, false);
      }
    };

    let ticking = false;
    window.addEventListener("scroll", () => {
      if (!ticking) {
        window.requestAnimationFrame(() => {
          updateScrollSpy(true);
          ticking = false;
        });
        ticking = true;
      }
    }, { passive: true });

    window.addEventListener("resize", () => {
      if (currentActiveLink) updateIndicator(currentActiveLink);
      updateScrollSpy(false);
    });

    tocNav.addEventListener("click", event => {
      const link = event.target.closest("a");
      if (!link) return;
      const href = link.getAttribute("href");
      if (!href || !href.startsWith("#") || href === "#") return;
      const targetId = decodeURIComponent(href.slice(1));
      if (linkMap.has(targetId)) {
        setActiveLink(link, true);
      }
    });

    updateScrollSpy(false);
    if (document.fonts) {
      document.fonts.ready.then(() => {
        if (currentActiveLink) updateIndicator(currentActiveLink);
        updateScrollSpy(false);
      });
    }
  }
}
