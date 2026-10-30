// colorMode.ts — the portal's light/dark switch.
//
// The mode lives on <html data-theme>, which styles/jarvis-tokens.css reads.
// index.html sets it before first paint from localStorage('jarvis_mode'); with
// no saved choice the OS preference applies.

import { useCallback, useEffect, useState } from 'react'

export type ColorMode = 'light' | 'dark'

const KEY = 'jarvis_mode'

const current = (): ColorMode => {
  const set = document.documentElement.getAttribute('data-theme')
  if (set === 'light' || set === 'dark') return set
  return window.matchMedia?.('(prefers-color-scheme: light)').matches ? 'light' : 'dark'
}

export function useColorMode(): [ColorMode, () => void] {
  const [mode, setMode] = useState<ColorMode>(current)

  useEffect(() => {
    const mq = window.matchMedia?.('(prefers-color-scheme: light)')
    if (!mq) return
    const onChange = () => setMode(current())
    mq.addEventListener('change', onChange)
    return () => mq.removeEventListener('change', onChange)
  }, [])

  const toggle = useCallback(() => {
    const next: ColorMode = current() === 'dark' ? 'light' : 'dark'
    document.documentElement.setAttribute('data-theme', next)
    try {
      localStorage.setItem(KEY, next)
    } catch {
      /* private mode: the choice lasts this page only */
    }
    setMode(next)
  }, [])

  return [mode, toggle]
}
