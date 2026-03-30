/**
 * ansi.js — thin wrapper around the ansi-to-html package.
 * Re-exports a pre-configured convert() function ready for use with
 * unsafeHTML() in Lit components.
 *
 * ansi-to-html: https://github.com/rburns/ansi-to-html
 */
import Convert from 'https://esm.sh/ansi-to-html@0.7.2';

// Catppuccin Mocha palette mapped to the 16 ANSI colour slots.
const convert = new Convert({
  fg:      '#cdd6f4',
  bg:      '#1e1e2e',
  newline: false,
  escapeXML: true,
  colors: {
    0:  '#585b70', // black   → surface2
    1:  '#f38ba8', // red
    2:  '#a6e3a1', // green
    3:  '#f9e2af', // yellow
    4:  '#89b4fa', // blue
    5:  '#cba6f7', // magenta
    6:  '#89dceb', // cyan
    7:  '#cdd6f4', // white
    8:  '#6c7086', // bright black → overlay0
    9:  '#f38ba8', // bright red
    10: '#a6e3a1', // bright green
    11: '#f9e2af', // bright yellow
    12: '#89b4fa', // bright blue
    13: '#cba6f7', // bright magenta
    14: '#89dceb', // bright cyan
    15: '#cdd6f4', // bright white
  },
});

/**
 * Convert a string containing ANSI escape sequences to an HTML string.
 * Text is HTML-escaped by ansi-to-html; safe for unsafeHTML / innerHTML.
 *
 * @param {string} text
 * @returns {string}
 */
export function ansiToHtml(text) {
  return convert.toHtml(text);
}
