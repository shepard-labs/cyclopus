/* Cyclopus theme engine — eight canonical Tailwind theme presets.
   Applied as CSS variables on <html>, persisted to localStorage, and synced
   after htmx body swaps. */
(function () {
  var THEMES = {
    obsidian: {
      label: 'Obsidian', dark: true,
      desc: 'True black · teal accent · sharp hairlines',
      vars: {
        '--color-base-100': '#0A0A0B', '--color-base-200': '#111113', '--color-base-300': '#18181B',
        '--color-base-content': '#FAFAFA',
		'--color-line': 'rgba(255,255,255,0.06)',
        '--color-primary': '#2DD4BF', '--color-primary-content': '#04211D',
        '--color-secondary': '#18181B', '--color-secondary-content': '#FAFAFA',
        '--color-accent': '#5EEAD4',
        '--color-neutral': '#A1A1AA', '--color-neutral-content': '#09090B',
        '--color-info': '#60A5FA', '--color-info-content': '#082F49',
        '--color-success': '#34D399', '--color-success-content': '#022C22',
        '--color-warning': '#FBBF24', '--color-warning-content': '#451A03',
        '--color-error': '#FB7185', '--color-error-content': '#450A0A',
        '--radius-selector': '0.25rem', '--radius-field': '6px', '--radius-box': '8px',
		'--shadow': '0 8px 30px rgb(0 0 0 / 40%)',
		'--shadow-auth-glow': '0 8px 30px rgba(0,0,0,0.40), 0 0 0 1px rgba(45,212,191,0.35), 0 0 40px -10px rgba(45,212,191,0.25)',
		'--font-sans': "'Space Grotesk', ui-sans-serif, system-ui, sans-serif, 'Apple Color Emoji', 'Segoe UI Emoji', 'Segoe UI Symbol', 'Noto Color Emoji'", '--font-mono': "'JetBrains Mono', ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, 'Liberation Mono', 'Courier New', monospace"
      }
    },
    mono: {
      label: 'Mono', dark: true,
      desc: 'Pure black & white · blue accent · zero radius',
      vars: {
        '--color-base-100': '#000000', '--color-base-200': '#0A0A0A', '--color-base-300': '#171717',
        '--color-base-content': '#FFFFFF',
		'--color-line': 'rgba(255,255,255,0.10)',
        '--color-primary': '#3B82F6', '--color-primary-content': '#000000',
        '--color-secondary': '#171717', '--color-secondary-content': '#FFFFFF',
        '--color-accent': '#60A5FA',
        '--color-neutral': '#A3A3A3', '--color-neutral-content': '#000000',
        '--color-info': '#60A5FA', '--color-info-content': '#082F49',
        '--color-success': '#22C55E', '--color-success-content': '#052E16',
        '--color-warning': '#F59E0B', '--color-warning-content': '#451A03',
        '--color-error': '#EF4444', '--color-error-content': '#FFFFFF',
        '--radius-selector': '0px', '--radius-field': '0px', '--radius-box': '0px',
		'--shadow': '0 8px 30px rgb(0 0 0 / 40%)',
		'--shadow-auth-glow': '0 8px 30px rgba(0,0,0,0.40), 0 0 0 1px rgba(59,130,246,0.35), 0 0 40px -10px rgba(59,130,246,0.25)',
		'--font-sans': "'Inter', ui-sans-serif, system-ui, sans-serif, 'Apple Color Emoji', 'Segoe UI Emoji', 'Segoe UI Symbol', 'Noto Color Emoji'", '--font-mono': "'JetBrains Mono', ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, 'Liberation Mono', 'Courier New', monospace"
      }
    },
    ember: {
      label: 'Ember', dark: true,
      desc: 'Warm stone · orange glow · soft rounded cards',
      vars: {
        '--color-base-100': '#1C1917', '--color-base-200': '#292524', '--color-base-300': '#44403C',
        '--color-base-content': '#FAFAF9',
		'--color-line': 'rgba(250,250,249,0.08)',
        '--color-primary': '#FB923C', '--color-primary-content': '#431407',
        '--color-secondary': '#292524', '--color-secondary-content': '#FAFAF9',
        '--color-accent': '#FDBA74',
        '--color-neutral': '#D6D3D1', '--color-neutral-content': '#1C1917',
        '--color-info': '#60A5FA', '--color-info-content': '#082F49',
        '--color-success': '#4ADE80', '--color-success-content': '#052E16',
        '--color-warning': '#FBBF24', '--color-warning-content': '#451A03',
        '--color-error': '#FB7185', '--color-error-content': '#450A0A',
        '--radius-selector': '0.25rem', '--radius-field': '8px', '--radius-box': '12px',
		'--shadow': '0 8px 30px rgb(0 0 0 / 40%)',
		'--shadow-auth-glow': '0 8px 30px rgba(0,0,0,0.40), 0 0 0 1px rgba(251,146,60,0.35), 0 0 40px -10px rgba(251,146,60,0.25)',
		'--font-sans': "'Outfit', ui-sans-serif, system-ui, sans-serif, 'Apple Color Emoji', 'Segoe UI Emoji', 'Segoe UI Symbol', 'Noto Color Emoji'", '--font-mono': "'JetBrains Mono', ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, 'Liberation Mono', 'Courier New', monospace"
      }
    },
    graphite: {
      label: 'Graphite', dark: true,
      desc: 'Indigo-tinted ink · muted · compact density',
      vars: {
        '--color-base-100': '#101012', '--color-base-200': '#17171A', '--color-base-300': '#1F1F23',
        '--color-base-content': '#F4F4F5',
		'--color-line': 'rgba(255,255,255,0.07)',
        '--color-primary': '#5E6AD2', '--color-primary-content': '#FFFFFF',
        '--color-secondary': '#1F1F23', '--color-secondary-content': '#F4F4F5',
        '--color-accent': '#8B93E8',
        '--color-neutral': '#9D9DA6', '--color-neutral-content': '#101012',
        '--color-info': '#60A5FA', '--color-info-content': '#082F49',
        '--color-success': '#4CB782', '--color-success-content': '#06281B',
        '--color-warning': '#E5C07B', '--color-warning-content': '#3A2A05',
        '--color-error': '#E5534B', '--color-error-content': '#FFFFFF',
        '--radius-selector': '4px', '--radius-field': '5px', '--radius-box': '6px',
		'--shadow': '0 8px 30px rgb(0 0 0 / 40%)',
		'--shadow-auth-glow': '0 8px 30px rgba(0,0,0,0.40), 0 0 0 1px rgba(94,106,210,0.35), 0 0 40px -10px rgba(94,106,210,0.25)',
		'--font-sans': "'Inter', ui-sans-serif, system-ui, sans-serif, 'Apple Color Emoji', 'Segoe UI Emoji', 'Segoe UI Symbol', 'Noto Color Emoji'", '--font-mono': "'IBM Plex Mono', ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, 'Liberation Mono', 'Courier New', monospace"
      }
    },
    terminal: {
      label: 'Terminal', dark: true,
      desc: 'Monospace UI · phosphor green · brutalist',
      vars: {
        '--color-base-100': '#0C0C0C', '--color-base-200': '#111111', '--color-base-300': '#1A1A1A',
        '--color-base-content': '#EDEDED',
		'--color-line': 'rgba(255,255,255,0.12)',
        '--color-primary': '#00DC82', '--color-primary-content': '#052E16',
        '--color-secondary': '#1A1A1A', '--color-secondary-content': '#EDEDED',
        '--color-accent': '#5CFFC0',
        '--color-neutral': '#8A8A8A', '--color-neutral-content': '#0C0C0C',
        '--color-info': '#00DC82', '--color-info-content': '#052E16',
        '--color-success': '#00DC82', '--color-success-content': '#052E16',
        '--color-warning': '#FBBF24', '--color-warning-content': '#451A03',
        '--color-error': '#FF5F56', '--color-error-content': '#FFFFFF',
        '--radius-selector': '0px', '--radius-field': '0px', '--radius-box': '0px',
		'--shadow': '0 8px 30px rgb(0 0 0 / 40%)',
		'--shadow-auth-glow': '0 8px 30px rgba(0,0,0,0.40), 0 0 0 1px rgba(0,220,130,0.35), 0 0 40px -10px rgba(0,220,130,0.25)',
		'--font-sans': "'JetBrains Mono', ui-sans-serif, system-ui, sans-serif, 'Apple Color Emoji', 'Segoe UI Emoji', 'Segoe UI Symbol', 'Noto Color Emoji'", '--font-mono': "'JetBrains Mono', ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, 'Liberation Mono', 'Courier New', monospace"
      }
    },
    paper: {
      label: 'Paper', dark: false,
      desc: 'Warm white · teal accent · soft hairlines',
      vars: {
        '--color-base-100': '#FAFAF9', '--color-base-200': '#FFFFFF', '--color-base-300': '#F5F5F4',
        '--color-base-content': '#1C1917',
		'--color-line': 'rgba(28,25,23,0.08)',
        '--color-primary': '#0D9488', '--color-primary-content': '#1C1917',
        '--color-secondary': '#F5F5F4', '--color-secondary-content': '#1C1917',
        '--color-accent': '#0D9488',
        '--color-neutral': '#57534E', '--color-neutral-content': '#FAFAF9',
        '--color-info': '#0284C7', '--color-info-content': '#FFFFFF',
        '--color-success': '#059669', '--color-success-content': '#FFFFFF',
        '--color-warning': '#D97706', '--color-warning-content': '#FFFFFF',
        '--color-error': '#E11D48', '--color-error-content': '#FFFFFF',
        '--radius-selector': '0.25rem', '--radius-field': '7px', '--radius-box': '10px',
		'--shadow': '0 8px 30px rgb(0 0 0 / 18%)',
		'--shadow-auth-glow': '0 8px 30px rgba(0,0,0,0.18), 0 0 0 1px rgba(13,148,136,0.35), 0 0 40px -10px rgba(13,148,136,0.25)',
		'--font-sans': "'Space Grotesk', ui-sans-serif, system-ui, sans-serif, 'Apple Color Emoji', 'Segoe UI Emoji', 'Segoe UI Symbol', 'Noto Color Emoji'", '--font-mono': "'JetBrains Mono', ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, 'Liberation Mono', 'Courier New', monospace"
      }
    },
    console: {
      label: 'Console', dark: false,
      desc: 'Cloud-console blue-gray · dense · tabular',
      vars: {
        '--color-base-100': '#F1F3F4', '--color-base-200': '#FFFFFF', '--color-base-300': '#E8EAED',
        '--color-base-content': '#202124',
		'--color-line': 'rgba(32,33,36,0.10)',
        '--color-primary': '#1A73E8', '--color-primary-content': '#FFFFFF',
        '--color-secondary': '#E8EAED', '--color-secondary-content': '#202124',
        '--color-accent': '#1A73E8',
        '--color-neutral': '#5F6368', '--color-neutral-content': '#FFFFFF',
        '--color-info': '#1A73E8', '--color-info-content': '#FFFFFF',
        '--color-success': '#188038', '--color-success-content': '#FFFFFF',
        '--color-warning': '#F9AB00', '--color-warning-content': '#202124',
        '--color-error': '#D93025', '--color-error-content': '#FFFFFF',
        '--radius-selector': '4px', '--radius-field': '4px', '--radius-box': '6px',
		'--shadow': '0 8px 30px rgb(0 0 0 / 18%)',
		'--shadow-auth-glow': '0 8px 30px rgba(0,0,0,0.18), 0 0 0 1px rgba(26,115,232,0.35), 0 0 40px -10px rgba(26,115,232,0.25)',
		'--font-sans': "'Inter', ui-sans-serif, system-ui, sans-serif, 'Apple Color Emoji', 'Segoe UI Emoji', 'Segoe UI Symbol', 'Noto Color Emoji'", '--font-mono': "'IBM Plex Mono', ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, 'Liberation Mono', 'Courier New', monospace"
      }
    },
    solar: {
      label: 'Solar', dark: false,
      desc: 'Solarized cream · low glare · long sessions',
      vars: {
        '--color-base-100': '#FDF6E3', '--color-base-200': '#F5EFDC', '--color-base-300': '#EEE8D5',
        '--color-base-content': '#073642',
		'--color-line': 'rgba(7,54,66,0.10)',
        '--color-primary': '#2AA198', '--color-primary-content': '#002B36',
        '--color-secondary': '#EEE8D5', '--color-secondary-content': '#073642',
        '--color-accent': '#2AA198',
        '--color-neutral': '#586E75', '--color-neutral-content': '#FDF6E3',
        '--color-info': '#268BD2', '--color-info-content': '#FDF6E3',
        '--color-success': '#859900', '--color-success-content': '#FDF6E3',
        '--color-warning': '#B58900', '--color-warning-content': '#FDF6E3',
        '--color-error': '#DC322F', '--color-error-content': '#FDF6E3',
        '--radius-selector': '0.25rem', '--radius-field': '6px', '--radius-box': '8px',
		'--shadow': '0 8px 30px rgb(0 0 0 / 18%)',
		'--shadow-auth-glow': '0 8px 30px rgba(0,0,0,0.18), 0 0 0 1px rgba(42,161,152,0.35), 0 0 40px -10px rgba(42,161,152,0.25)',
		'--font-sans': "'Outfit', ui-sans-serif, system-ui, sans-serif, 'Apple Color Emoji', 'Segoe UI Emoji', 'Segoe UI Symbol', 'Noto Color Emoji'", '--font-mono': "'JetBrains Mono', ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, 'Liberation Mono', 'Courier New', monospace"
      }
    }
  };

  var KEY = 'cyclopus-theme-v2';
  var DEFAULT_DARK = 'obsidian';
  var DEFAULT_LIGHT = 'paper';
  var TAILWIND_ALIASES = {
    '--tw-color-base-100': '--color-base-100',
    '--tw-color-base-200': '--color-base-200',
    '--tw-color-base-300': '--color-base-300',
    '--tw-color-base-content': '--color-base-content',
    '--tw-color-primary': '--color-primary',
    '--tw-color-primary-content': '--color-primary-content',
    '--tw-color-secondary': '--color-secondary',
    '--tw-color-secondary-content': '--color-secondary-content',
    '--tw-color-accent': '--color-accent',
    '--tw-color-neutral': '--color-neutral',
    '--tw-color-neutral-content': '--color-neutral-content',
    '--tw-color-info': '--color-info',
    '--tw-color-info-content': '--color-info-content',
    '--tw-color-success': '--color-success',
    '--tw-color-success-content': '--color-success-content',
    '--tw-color-warning': '--color-warning',
    '--tw-color-warning-content': '--color-warning-content',
    '--tw-color-error': '--color-error',
    '--tw-color-error-content': '--color-error-content',
	'--tw-color-line': '--color-line',
    '--tw-radius-selector': '--radius-selector',
    '--tw-radius-field': '--radius-field',
    '--tw-radius-box': '--radius-box',
    '--tw-font-sans': '--font-sans',
    '--tw-font-mono': '--font-mono',
	'--tw-shadow-theme': '--shadow',
	'--tw-shadow-auth-glow': '--shadow-auth-glow'
  };

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
    var themeName = isTheme(name) ? name : DEFAULT_DARK;
    var theme = THEMES[themeName];
    var root = document.documentElement;
    for (var token in theme.vars) root.style.setProperty(token, theme.vars[token]);
    for (var alias in TAILWIND_ALIASES) {
      root.style.setProperty(alias, 'var(' + TAILWIND_ALIASES[alias] + ')');
    }
    root.setAttribute('data-theme', themeName);
    root.setAttribute('data-mode', theme.dark ? 'dark' : 'light');
    storageSet(KEY, themeName);
    storageSet(KEY + (theme.dark ? '-dark' : '-light'), themeName);
    syncChrome(themeName);
  }
  function toggle() {
    var name = current();
    var targetDark = !THEMES[name].dark;
    var remembered = storageGet(KEY + (targetDark ? '-dark' : '-light'));
    apply(targetDark
      ? (isTheme(remembered) && THEMES[remembered].dark ? remembered : DEFAULT_DARK)
      : (isTheme(remembered) && !THEMES[remembered].dark ? remembered : DEFAULT_LIGHT));
  }
  function syncChrome(name) {
    var themeName = name || current();
    var theme = THEMES[themeName];
    var icons = document.querySelectorAll('[data-theme-icon]');
    for (var i = 0; i < icons.length; i++) icons[i].textContent = theme.dark ? '☾' : '☀';
    var pickers = document.querySelectorAll('[data-theme-name]');
    for (var j = 0; j < pickers.length; j++) {
      var element = pickers[j];
      var selected = element.getAttribute('data-theme-name') === themeName;
      element.setAttribute('aria-pressed', selected ? 'true' : 'false');
    }
  }
  function sync() { syncChrome(current()); }

  var scope = typeof window !== 'undefined' ? window : {};
  scope.cyclopusTheme = {
    apply: apply,
    toggle: toggle,
    current: current,
    sync: sync,
    themes: THEMES
  };

  apply(current());
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', sync);
  } else {
    sync();
  }
  document.addEventListener('htmx:load', sync);
})();
