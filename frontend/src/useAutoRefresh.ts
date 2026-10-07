import { useEffect } from 'react'

export function useAutoRefresh(refresh: () => void | Promise<unknown>, enabled = true, interval = 5000, immediate = true) {
  useEffect(() => {
    if (!enabled) return
    let active = true
    let inFlight = false
    const run = async () => {
      if (!active || document.hidden || inFlight) return
      inFlight = true
      try { await refresh() } finally { inFlight = false }
    }
    if (immediate) void run()
    const timer = window.setInterval(() => { void run() }, interval)
    const onReturn = () => { void run() }
    document.addEventListener('visibilitychange', onReturn)
    window.addEventListener('focus', onReturn)
    return () => {
      active = false
      window.clearInterval(timer)
      document.removeEventListener('visibilitychange', onReturn)
      window.removeEventListener('focus', onReturn)
    }
  }, [refresh, enabled, interval, immediate])
}
