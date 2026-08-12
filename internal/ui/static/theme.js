/* Cyclopus theme engine — 8 full-token presets ported from design-artifacts/mockups.
   Every preset covers the COMPLETE daisyUI token set the CSS consumes: base surfaces,
   brand, semantic states + their content colors, neutral, radius scale. Applied as
   CSS variables on <html>; persisted to localStorage.

   Lifecycle: apply() runs at script top level (script is loaded synchronously in
   <head> before ui.css — prevents a flash of the default theme). sync() runs on
   DOMContentLoaded AND htmx:load because hx-boost swaps <body> on navigation,
   leaving server-rendered [data-theme-icon] / [data-theme-name] state stale. */
(function () {
  /* Each preset: label, desc, dark flag, vars (complete daisyUI token set). */
  var THEMES = {
    /* 1 — the original: true-black, teal+violet, sharp hairlines */
    obsidian: {
      label: 'Obsidian', dark: true,
      desc: 'True black · teal accent · sharp hairlines',
      vars: {
        '--color-base-100': '#0A0A0B', '--color-base-200': '#111113', '--color-base-300': '#18181B',
        '--color-base-content': '#FAFAFA',
        '--color-primary': '#2DD4BF', '--color-primary-content': '#04211D',
        '--color-secondary': '#18181B', '--color-secondary-content': '#FAFAFA',
        '--color-accent': '#5EEAD4',
        '--color-neutral': '#A1A1AA', '--color-neutral-content': '#09090B',
        '--color-info': '#60A5FA', '--color-info-content': '#082F49',
        '--color-success': '#34D399', '--color-success-content': '#022C22',
        '--color-warning': '#FBBF24', '--color-warning-content': '#451A03',
        '--color-error': '#FB7185', '--color-error-content': '#450A0A',
        '--radius-selector': '0.25rem', '--radius-field': '6px', '--radius-box': '8px',
        '--shadow': 'rgb(0 0 0 / 40%)',
        '--font-sans': "'Space Grotesk'", '--font-mono': "'JetBrains Mono'"
      }
    },
    /* 2 — Vercel-adjacent: pure b/w, blue accent, zero radius */
    mono: {
      label: 'Mono', dark: true,
      desc: 'Pure black & white · blue accent · zero radius',
      vars: {
        '--color-base-100': '#000000', '--color-base-200': '#0A0A0A', '--color-base-300': '#171717',
        '--color-base-content': '#FFFFFF',
        '--color-primary': '#3B82F6', '--color-primary-content': '#FFFFFF',
        '--color-secondary': '#171717', '--color-secondary-content': '#FFFFFF',
        '--color-accent': '#60A5FA',
        '--color-neutral': '#A3A3A3', '--color-neutral-content': '#000000',
        '--color-info': '#60A5FA', '--color-info-content': '#082F49',
        '--color-success': '#22C55E', '--color-success-content': '#052E16',
        '--color-warning': '#F59E0B', '--color-warning-content': '#451A03',
        '--color-error': '#EF4444', '--color-error-content': '#FFFFFF',
        '--radius-selector': '0px', '--radius-field': '0px', '--radius-box': '0px',
        '--shadow': 'rgb(0 0 0 / 40%)',
        '--font-sans': "'Inter'", '--font-mono': "'JetBrains Mono'"
      }
    },
    /* 3 — warm stone + orange, rounded, friendly SaaS */
    ember: {
      label: 'Ember', dark: true,
      desc: 'Warm stone · orange glow · soft rounded cards',
      vars: {
        '--color-base-100': '#1C1917', '--color-base-200': '#292524', '--color-base-300': '#44403C',
        '--color-base-content': '#FAFAF9',
        '--color-primary': '#FB923C', '--color-primary-content': '#431407',
        '--color-secondary': '#292524', '--color-secondary-content': '#FAFAF9',
        '--color-accent': '#FDBA74',
        '--color-neutral': '#D6D3D1', '--color-neutral-content': '#1C1917',
        '--color-info': '#60A5FA', '--color-info-content': '#082F49',
        '--color-success': '#4ADE80', '--color-success-content': '#052E16',
        '--color-warning': '#FBBF24', '--color-warning-content': '#451A03',
        '--color-error': '#FB7185', '--color-error-content': '#450A0A',
        '--radius-selector': '0.25rem', '--radius-field': '8px', '--radius-box': '12px',
        '--shadow': 'rgb(0 0 0 / 40%)',
        '--font-sans': "'Outfit'", '--font-mono': "'JetBrains Mono'"
      }
    },
    /* 4 — Linear-adjacent: indigo ink, muted, compact */
    graphite: {
      label: 'Graphite', dark: true,
      desc: 'Indigo-tinted ink · muted · compact density',
      vars: {
        '--color-base-100': '#101012', '--color-base-200': '#17171A', '--color-base-300': '#1F1F23',
        '--color-base-content': '#F4F4F5',
        '--color-primary': '#5E6AD2', '--color-primary-content': '#FFFFFF',
        '--color-secondary': '#1F1F23', '--color-secondary-content': '#F4F4F5',
        '--color-accent': '#8B93E8',
        '--color-neutral': '#9D9DA6', '--color-neutral-content': '#101012',
        '--color-info': '#60A5FA', '--color-info-content': '#082F49',
        '--color-success': '#4CB782', '--color-success-content': '#06281B',
        '--color-warning': '#E5C07B', '--color-warning-content': '#3A2A05',
        '--color-error': '#E5534B', '--color-error-content': '#FFFFFF',
        '--radius-selector': '4px', '--radius-field': '5px', '--radius-box': '6px',
        '--shadow': 'rgb(0 0 0 / 40%)',
        '--font-sans': "'Inter'", '--font-mono': "'IBM Plex Mono'"
      }
    },
    /* 5 — terminal-forward: monospace UI, green phosphor */
    terminal: {
      label: 'Terminal', dark: true,
      desc: 'Monospace UI · phosphor green · brutalist',
      vars: {
        '--color-base-100': '#0C0C0C', '--color-base-200': '#111111', '--color-base-300': '#1A1A1A',
        '--color-base-content': '#EDEDED',
        '--color-primary': '#00DC82', '--color-primary-content': '#052E16',
        '--color-secondary': '#1A1A1A', '--color-secondary-content': '#EDEDED',
        '--color-accent': '#5CFFC0',
        '--color-neutral': '#8A8A8A', '--color-neutral-content': '#0C0C0C',
        '--color-info': '#00DC82', '--color-info-content': '#052E16',
        '--color-success': '#00DC82', '--color-success-content': '#052E16',
        '--color-warning': '#FBBF24', '--color-warning-content': '#451A03',
        '--color-error': '#FF5F56', '--color-error-content': '#FFFFFF',
        '--radius-selector': '0px', '--radius-field': '0px', '--radius-box': '0px',
        '--shadow': 'rgb(0 0 0 / 40%)',
        '--font-sans': "'JetBrains Mono'", '--font-mono': "'JetBrains Mono'"
      }
    },
    /* 6 — light: warm paper, teal keeps brand continuity with Obsidian */
    paper: {
      label: 'Paper', dark: false,
      desc: 'Warm white · teal accent · soft hairlines',
      vars: {
        '--color-base-100': '#FAFAF9', '--color-base-200': '#FFFFFF', '--color-base-300': '#F5F5F4',
        '--color-base-content': '#1C1917',
        '--color-primary': '#0D9488', '--color-primary-content': '#FFFFFF',
        '--color-secondary': '#F5F5F4', '--color-secondary-content': '#1C1917',
        '--color-accent': '#0D9488',
        '--color-neutral': '#57534E', '--color-neutral-content': '#FAFAF9',
        '--color-info': '#0284C7', '--color-info-content': '#FFFFFF',
        '--color-success': '#059669', '--color-success-content': '#FFFFFF',
        '--color-warning': '#D97706', '--color-warning-content': '#FFFFFF',
        '--color-error': '#E11D48', '--color-error-content': '#FFFFFF',
        '--radius-selector': '0.25rem', '--radius-field': '7px', '--radius-box': '10px',
        '--shadow': 'rgb(0 0 0 / 18%)',
        '--font-sans': "'Space Grotesk'", '--font-mono': "'JetBrains Mono'"
      }
    },
    /* 7 — light: GCP/console feel, blue-gray, dense */
    console: {
      label: 'Console', dark: false,
      desc: 'Cloud-console blue-gray · dense · tabular',
      vars: {
        '--color-base-100': '#F1F3F4', '--color-base-200': '#FFFFFF', '--color-base-300': '#E8EAED',
        '--color-base-content': '#202124',
        '--color-primary': '#1A73E8', '--color-primary-content': '#FFFFFF',
        '--color-secondary': '#E8EAED', '--color-secondary-content': '#202124',
        '--color-accent': '#1A73E8',
        '--color-neutral': '#5F6368', '--color-neutral-content': '#FFFFFF',
        '--color-info': '#1A73E8', '--color-info-content': '#FFFFFF',
        '--color-success': '#188038', '--color-success-content': '#FFFFFF',
        '--color-warning': '#F9AB00', '--color-warning-content': '#202124',
        '--color-error': '#D93025', '--color-error-content': '#FFFFFF',
        '--radius-selector': '4px', '--radius-field': '4px', '--radius-box': '6px',
        '--shadow': 'rgb(0 0 0 / 18%)',
        '--font-sans': "'Inter'", '--font-mono': "'IBM Plex Mono'"
      }
    },
    /* 8 — light: Solarized, low-glare, long-session friendly */
    solar: {
      label: 'Solar', dark: false,
      desc: 'Solarized cream · low glare · long sessions',
      vars: {
        '--color-base-100': '#FDF6E3', '--color-base-200': '#F5EFDC', '--color-base-300': '#EEE8D5',
        '--color-base-content': '#073642',
        '--color-primary': '#2AA198', '--color-primary-content': '#FDF6E3',
        '--color-secondary': '#EEE8D5', '--color-secondary-content': '#073642',
        '--color-accent': '#2AA198',
        '--color-neutral': '#586E75', '--color-neutral-content': '#FDF6E3',
        '--color-info': '#268BD2', '--color-info-content': '#FDF6E3',
        '--color-success': '#859900', '--color-success-content': '#FDF6E3',
        '--color-warning': '#B58900', '--color-warning-content': '#FDF6E3',
        '--color-error': '#DC322F', '--color-error-content': '#FDF6E3',
        '--radius-selector': '0.25rem', '--radius-field': '6px', '--radius-box': '8px',
        '--shadow': 'rgb(0 0 0 / 18%)',
        '--font-sans': "'Outfit'", '--font-mono': "'JetBrains Mono'"
      }
    }
  };

  var KEY = 'cyclopus-theme-v2';
  var DEFAULT_DARK = 'obsidian';
  var DEFAULT_LIGHT = 'paper';

  function storageGet(key) {
    try { return localStorage.getItem(key); } catch (e) { return null; }
  }
  function storageSet(key, value) {
    try { localStorage.setItem(key, value); } catch (e) {}
  }

  function isTheme(name) { return Object.prototype.hasOwnProperty.call(THEMES, name); }

  function current() {
    var saved = storageGet(KEY);
    return isTheme(saved) ? saved : DEFAULT_DARK;
  }

  function apply(name) {
    var t = THEMES[name] || THEMES[DEFAULT_DARK];
    var r = document.documentElement;
    for (var k in t.vars) r.style.setProperty(k, t.vars[k]);
    r.setAttribute('data-theme', name);
    r.setAttribute('data-mode', t.dark ? 'dark' : 'light');
    storageSet(KEY, name);
    /* Remember per-mode so toggle() can restore the user's last dark/light pick. */
    storageSet(KEY + (t.dark ? '-dark' : '-light'), name);
    syncChrome(name);
  }

  /* Dark↔light pair toggle: flips the mode, restoring the last preset used in
     the target mode (defaults: obsidian / paper). */
  function toggle() {
    var name = current();
    var targetDark = !THEMES[name].dark;
    var remembered = storageGet(KEY + (targetDark ? '-dark' : '-light'));
    apply(targetDark
      ? (isTheme(remembered) && THEMES[remembered].dark ? remembered : DEFAULT_DARK)
      : (isTheme(remembered) && !THEMES[remembered].dark ? remembered : DEFAULT_LIGHT));
  }

  /* Re-sync JS-dependent chrome after body swaps (htmx:load) or first paint.
     Server-rendered markup cannot know the persisted theme, so icons and
     selected states are corrected here. */
  function syncChrome(name) {
    var themeName = name || current();
    var t = THEMES[themeName];
    var icons = document.querySelectorAll('[data-theme-icon]');
    for (var i = 0; i < icons.length; i++) icons[i].textContent = t.dark ? '☾' : '☀';
    var pickers = document.querySelectorAll('[data-theme-name]');
    for (var j = 0; j < pickers.length; j++) {
      var el = pickers[j];
      var selected = el.getAttribute('data-theme-name') === themeName;
      el.classList.toggle('theme-option-selected', selected);
      el.setAttribute('aria-pressed', selected ? 'true' : 'false');
    }
  }

  function sync() { syncChrome(current()); }

  var w = typeof window !== 'undefined' ? window : {};
  w.cyclopusTheme = {
    apply: apply,
    toggle: toggle,
    current: current,
    sync: sync,
    themes: THEMES
  };

  /* Apply immediately — this script is loaded synchronously in <head> before the
     stylesheet, so the first paint already uses the persisted theme. */
  apply(current());

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', sync);
  } else {
    sync();
  }
  document.addEventListener('htmx:load', sync);
})();
