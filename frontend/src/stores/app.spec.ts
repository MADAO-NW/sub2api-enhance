import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useAppStore } from './app'

describe('global toast', () => {
  beforeEach(() => { setActivePinia(createPinia()); vi.useFakeTimers() })

  it('auto dismisses after three seconds and resets the timer for a new message', () => {
    const app = useAppStore()
    app.showSuccess('saved')
    vi.advanceTimersByTime(2000)
    app.showError('failed')
    vi.advanceTimersByTime(2000)
    expect(app.message).toBe('failed')
    expect(app.error).toBe(true)
    vi.advanceTimersByTime(1000)
    expect(app.message).toBe('')
  })
})
