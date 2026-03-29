/**
 * ansi.js — convert ANSI SGR escape sequences to HTML spans.
 *
 * Colours are mapped to the Catppuccin Mocha palette used throughout the UI.
 * Text content is always HTML-escaped, so the output is safe to set as innerHTML.
 */

// Foreground colour map: ANSI code → Catppuccin Mocha hex.
const FG = {
  30: '#585b70', // black   → surface2
  31: '#f38ba8', // red
  32: '#a6e3a1', // green
  33: '#f9e2af', // yellow
  34: '#89b4fa', // blue
  35: '#cba6f7', // magenta
  36: '#89dceb', // cyan
  37: '#cdd6f4', // white
  90: '#6c7086', // bright black → overlay0
  91: '#f38ba8', // bright red
  92: '#a6e3a1', // bright green
  93: '#f9e2af', // bright yellow
  94: '#89b4fa', // bright blue
  95: '#cba6f7', // bright magenta
  96: '#89dceb', // bright cyan
  97: '#cdd6f4', // bright white
};

function escHtml(s) {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

/**
 * Convert a string that may contain ANSI SGR escape sequences into an HTML
 * string with equivalent <span style="…"> elements.
 *
 * Handles: reset (0), bold (1), italic (3), colour codes (30–37, 90–97).
 *
 * @param {string} text  Raw string, possibly containing \x1b[…m sequences.
 * @returns {string}     HTML-safe string ready for innerHTML / unsafeHTML.
 */
export function ansiToHtml(text) {
  // Split into alternating text segments and SGR sequences.
  const parts = text.split(/(\x1b\[[0-9;]*m)/);
  let out    = '';
  let bold   = false;
  let italic = false;
  let color  = '';

  for (const part of parts) {
    if (part.startsWith('\x1b[') && part.endsWith('m')) {
      // Parse each semicolon-separated code in the sequence.
      const seq   = part.slice(2, -1);
      const codes = seq === '' ? [0] : seq.split(';').map(Number);
      for (const c of codes) {
        if      (c === 0)              { bold = false; italic = false; color = ''; }
        else if (c === 1)              { bold   = true;  }
        else if (c === 3)              { italic = true;  }
        else if (c === 22)             { bold   = false; }
        else if (c === 23)             { italic = false; }
        else if (c === 39)             { color  = '';    }
        else if (FG[c] !== undefined)  { color  = FG[c]; }
      }
    } else if (part !== '') {
      const escaped = escHtml(part);
      if (bold || italic || color) {
        const styles = [];
        if (bold)   styles.push('font-weight:bold');
        if (italic) styles.push('font-style:italic');
        if (color)  styles.push('color:' + color);
        out += '<span style="' + styles.join(';') + '">' + escaped + '</span>';
      } else {
        out += escaped;
      }
    }
  }

  return out;
}
