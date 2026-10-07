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
    // only correct the small, status-bar-sized overflow this bug produces
    root.style.setProperty('--vb', over > 0 && over < 150 ? `${over}px` : '0px')
  }
  measure()
  addEventListener('resize', measure)
  addEventListener('orientationchange', () => setTimeout(measure, 300))
  addEventListener('pageshow', measure)
})()
