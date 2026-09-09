/* git.cmposer.cc client-side README renderer (render: client, R10-Q8 / R2-Q6).
 * Uses the vendored marked + highlight.js. Raw HTML embedded in markdown is
 * escaped, never rendered inline, so no README markup can inject script.
 */
(function () {
  'use strict';

  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  var renderer = new marked.Renderer();
  // Override the raw-HTML renderer: escape rather than emit (R2-Q6).
  renderer.html = function (html) {
    return escapeHtml(html);
  };
  marked.setOptions({
    renderer: renderer,
    gfm: true,
    breaks: false,
    pedantic: false
  });

  var nodes = document.querySelectorAll('.markdown[data-markdown]');
  for (var i = 0; i < nodes.length; i++) {
    var el = nodes[i];
    var src = el.getAttribute('data-markdown') || '';
    try {
      el.innerHTML = marked.parse(src);
      var blocks = el.querySelectorAll('pre code');
      for (var j = 0; j < blocks.length; j++) {
        hljs.highlightElement(blocks[j]);
      }
    } catch (e) {
      el.textContent = 'markdown render failed: ' + e.message;
    }
  }
})();
