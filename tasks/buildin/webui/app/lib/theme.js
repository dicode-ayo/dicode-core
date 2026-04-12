// Theme manager — persists the user's dark/light choice in localStorage,
// respects system preference on first visit, and dispatches a
// `dicode-theme-change` CustomEvent so components can react.
//
// Mirrors dicode-site/site/src/utils/theme.ts one-for-one (vanilla JS,
// no framework) so the behavior is identical across surfaces.

const STORAGE_KEY = 'dicode-theme';

export function getStoredTheme() {
  try {
    const v = localStorage.getItem(STORAGE_KEY);
    return v === 'light' || v === 'dark' ? v : null;
  } catch {
    return null;
  }
}

export function getSystemTheme() {
  if (typeof window === 'undefined' || !window.matchMedia) return 'dark';
  return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
}

export function getCurrentTheme() {
  return getStoredTheme() ?? getSystemTheme();
}

// Monaco has two built-in themes that match our palette closely enough
// for text-heavy editors. Components that embed monaco should call
// monacoTheme() when creating the editor and also subscribe to
// `dicode-theme-change` so they can call monaco.editor.setTheme() live.
export function monacoTheme() {
  return getCurrentTheme() === 'light' ? 'vs' : 'vs-dark';
}

export function applyTheme(theme) {
  document.documentElement.setAttribute('data-theme', theme);
  document.documentElement.style.colorScheme = theme;
  window.dispatchEvent(new CustomEvent('dicode-theme-change', { detail: theme }));
}

export function setTheme(theme) {
  try {
    localStorage.setItem(STORAGE_KEY, theme);
  } catch {
    // ignore storage errors (private mode etc.)
  }
  applyTheme(theme);
}

export function toggleTheme() {
  const next = getCurrentTheme() === 'dark' ? 'light' : 'dark';
  setTheme(next);
  return next;
}

// Call early (before first paint) to prevent FOUC. Also subscribes to
// system preference changes so users who haven't explicitly chosen
// follow their OS.
export function initTheme() {
  applyTheme(getCurrentTheme());
  if (!getStoredTheme() && window.matchMedia) {
    window.matchMedia('(prefers-color-scheme: light)').addEventListener('change', () => {
      if (!getStoredTheme()) applyTheme(getSystemTheme());
    });
  }
}
