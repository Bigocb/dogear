// iOS home-screen apps (standalone, translucent status bar) can lay the page out
// taller than the screen, which pushes anything pinned to the bottom (tab bar,
// sheets, the reader's controls) off the glass. Measure that overflow and expose
// it as --vb so bottom-anchored elements can be lifted back into view.
(function () {
  const root = document.documentElement
  function measure() {
    const standalone = navigator.standalone === true || matchMedia('(display-mode: standalone)').matches
    let over = 0
    if (standalone && screen.width && screen.height) {
      const portrait = matchMedia('(orientation: portrait)').matches
      const screenH = portrait ? Math.max(screen.width, screen.height) : Math.min(screen.width, screen.height)
      const layoutH = Math.max(window.innerHeight, root.clientHeight)
      over = Math.round(layoutH - screenH)
    }
    // only correct the small, status-bar-sized mismatch this bug produces.
    // Positive: layout taller than the screen (controls pushed off the glass),
    // lift them. Negative: layout shorter than the screen (a dead strip under
    // the controls), push them down by the shortfall.
    root.style.setProperty('--vb', Math.abs(over) < 150 ? `${over}px` : '0px')
  }
  measure()
  addEventListener('resize', measure)
  addEventListener('orientationchange', () => setTimeout(measure, 300))
  addEventListener('pageshow', measure)
})()
