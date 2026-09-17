import { useState, useMemo, useRef, useEffect, useCallback } from 'react'
import { Vehicle, CameraCalibration } from '../App'

// ── Server triage types ──────────────────────────────────────────────────────

interface TriageCycle {
  id: number
  created_at: string
  started_by_name: string
  started_by_email: string
  triage_date: string
  completed_at: string | null
}

interface TriageAnnotation {
  cam_quality: string
  cam_note: string
  drive_quality: string
  drive_note: string
  triage_comments: string
  included: boolean
  last_edited_by: string
  last_edited_at: string
}

interface ActionItem {
  content: string
  last_edited_by: string
  last_edited_at: string
}

interface ActiveEditor {
  name: string
  email: string
  started_at: string
  last_active: string
}

interface TriageState {
  cycle: TriageCycle | null
  annotations: Record<string, TriageAnnotation>
  action_items: Record<string, ActionItem>
  active_editor: ActiveEditor | null
  last_updated: string | null
  vehicles: Vehicle[] | null // null = use live prop; populated for historical cycles
}

type CamQuality = 'green' | 'yellow' | 'red' | ''
type DriveQuality = 'green' | 'red' | ''

// ── Quality popover button ───────────────────────────────────────────────────

type QualityType = 'cam' | 'drive'

const CAM_OPTIONS = [
  { value: 'green',  label: 'Good',     dotCls: 'bg-green-500' },
  { value: 'yellow', label: 'Degraded', dotCls: 'bg-amber-400' },
  { value: 'red',    label: 'Bad',      dotCls: 'bg-red-500' },
]
const DRIVE_OPTIONS = [
  { value: 'green', label: 'Good', dotCls: 'bg-green-500' },
  { value: 'red',   label: 'Bad',  dotCls: 'bg-red-500' },
]

const qualityDotCls = (q: string) =>
  q === 'green' ? 'bg-green-500' : q === 'yellow' ? 'bg-amber-400' : q === 'red' ? 'bg-red-500' : ''

const QualityCell = ({
  type,
  quality,
  note,
  disabled,
  onSave,
}: {
  type: QualityType
  quality: string
  note: string
  disabled?: boolean
  onSave: (quality: string, note: string) => void
}) => {
  const [open, setOpen] = useState(false)
  const [draftQuality, setDraftQuality] = useState(quality)
  const [draftNote, setDraftNote] = useState(note)
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (open) {
      setDraftQuality(quality)
      setDraftNote(note)
    }
  }, [open])  // intentional: sync from props only when popover opens

  useEffect(() => {
    if (!open) return
    const handler = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [open])

  const options = type === 'cam' ? CAM_OPTIONS : DRIVE_OPTIONS

  const handleSave = () => {
    onSave(draftQuality, draftNote)
    setOpen(false)
  }

  return (
    <div ref={ref} className="relative">
      <button
        onClick={() => !disabled && setOpen(o => !o)}
        disabled={disabled}
        className={`flex items-center gap-1.5 text-xs rounded px-1 py-0.5 min-w-[2rem] transition-colors ${
          disabled ? 'cursor-default opacity-60' : 'hover:bg-gray-100'
        }`}
      >
        {quality ? (
          <>
            <span className={`w-2.5 h-2.5 rounded-full flex-shrink-0 ${qualityDotCls(quality)}`} />
            {note && <span className="text-gray-600 line-clamp-1 max-w-[80px]">{note}</span>}
          </>
        ) : (
          <span className="text-gray-300">—</span>
        )}
      </button>

      {open && !disabled && (
        <div className="absolute left-0 top-full mt-1 z-50 bg-white border border-gray-200 rounded-lg shadow-xl p-3 w-52">
          <div className="flex gap-1.5 mb-2.5">
            {options.map(opt => (
              <button
                key={opt.value}
                onClick={() => setDraftQuality(v => v === opt.value ? '' : opt.value)}
                className={`flex-1 py-1.5 rounded-md text-xs font-medium flex items-center justify-center gap-1 transition-colors ${
                  draftQuality === opt.value
                    ? `${opt.dotCls} text-white`
                    : 'bg-gray-100 text-gray-600 hover:bg-gray-200'
                }`}
              >
                <span className={`w-2 h-2 rounded-full ${draftQuality === opt.value ? 'bg-white/60' : opt.dotCls}`} />
                {opt.label}
              </button>
            ))}
          </div>
          <input
            type="text"
            value={draftNote}
            onChange={e => setDraftNote(e.target.value)}
            onKeyDown={e => { if (e.key === 'Enter') handleSave() }}
            placeholder={type === 'cam' ? 'e.g. defocus, haze' : 'optional note'}
            className="w-full border border-gray-200 rounded px-2 py-1 text-xs mb-2 focus:outline-none focus:ring-1 focus:ring-primary"
            autoFocus
          />
          <div className="flex gap-1.5 justify-end">
            {quality && (
              <button
                onClick={() => { onSave('', ''); setOpen(false) }}
                className="text-xs text-gray-400 hover:text-red-500 px-2 py-1 rounded transition-colors"
              >
                Clear
              </button>
            )}
            <button
              onClick={() => setOpen(false)}
              className="text-xs text-gray-500 hover:bg-gray-100 px-2 py-1 rounded transition-colors"
            >
              Cancel
            </button>
            <button
              onClick={handleSave}
              className="text-xs bg-primary text-white px-2 py-1 rounded hover:bg-primary-700 transition-colors"
            >
              Save
            </button>
          </div>
        </div>
      )}
    </div>
  )
}

// ── Calibration columns ──────────────────────────────────────────────────────

const CALIBRATION_COLUMNS: { key: string; label: string }[] = [
  { key: 'front_center',        label: 'FC'  },
  { key: 'front_center_narrow', label: 'FCN' },
  { key: 'rear_center',         label: 'RC'  },
  { key: 'front_left',          label: 'FL'  },
  { key: 'front_right',         label: 'FR'  },
  { key: 'rear_left',           label: 'RL'  },
  { key: 'rear_right',          label: 'RR'  },
]

const CalibrationCell = ({ calibrations, camera }: { calibrations: CameraCalibration[]; camera: string }) => {
  const entry = calibrations.find(c => c.camera === camera)
  if (!entry) return <span className="text-gray-300 text-xs">—</span>
  const shortDate = entry.last_calibrated.slice(5).replace('-', '/')
  return (
    <span
      title={`${entry.last_calibrated} — ${entry.days_since}d ago${entry.stale ? ' (overdue)' : ''}`}
      className={`font-mono text-xs ${entry.stale ? 'text-red-600 font-semibold' : 'text-gray-500'}`}
    >
      {shortDate}
    </span>
  )
}

// ── Project / Location badges ────────────────────────────────────────────────

const PROJECT_GROUPS = ['All', 'Neuron', 'Oasis', 'Robotaxi'] as const
type ProjectGroup = typeof PROJECT_GROUPS[number]

const matchesGroup = (project: string | undefined, group: ProjectGroup) => {
  if (group === 'All') return true
  return (project || 'Neuron Gen-1').startsWith(group)
}

const projectStyle = (project: string): string => {
  if (project.startsWith('Oasis'))    return 'bg-blue-50 text-blue-700'
  if (project.startsWith('Robotaxi')) return 'bg-purple-50 text-purple-700'
  if (project.endsWith('Gen-2'))      return 'bg-green-50 text-green-700'
  return 'bg-gray-100 text-gray-600'
}

const ProjectBadge = ({ project }: { project?: string }) => {
  const label = project || 'Neuron Gen-1'
  return (
    <span className={`inline-flex px-2.5 py-1 rounded-md text-xs font-medium whitespace-nowrap ${projectStyle(label)}`}>
      {label}
    </span>
  )
}

const LocationBadge = ({ location }: { location?: string }) => {
  if (!location) return <span className="text-gray-300">—</span>
  return (
    <span className="inline-flex px-2.5 py-1 rounded-md text-xs font-medium whitespace-nowrap bg-slate-100 text-slate-600">
      {location}
    </span>
  )
}

// ── Helpers ──────────────────────────────────────────────────────────────────

const needsIncludeCheckbox = (v: Vehicle) => v.inactive || !v.run || v.run === ''

const defaultAnnot = (): TriageAnnotation => ({
  cam_quality: '',
  cam_note: '',
  drive_quality: '',
  drive_note: '',
  triage_comments: '',
  included: true,
  last_edited_by: '',
  last_edited_at: '',
})

const fmtDate = (iso: string | null | undefined): string => {
  if (!iso) return ''
  try {
    return new Date(iso).toLocaleString()
  } catch {
    return iso
  }
}

// ── Props ────────────────────────────────────────────────────────────────────

interface Props {
  vehicles: Vehicle[]
  isLoading: boolean
  isRefreshing: boolean
  error: string | null
  onRefresh: () => void
  onVehicleClick: (v: Vehicle, isHistorical?: boolean) => void
  projectFilter?: string
  latestCamTicketContent?: string
  onRefreshCamTickets?: () => void
  isRefreshingCamTickets?: boolean
  currentUserEmail?: string
}

// ── Action Items section ─────────────────────────────────────────────────────

const ACTION_SECTIONS = [
  { key: 'new',            label: 'New' },
  { key: 'ongoing',        label: 'Ongoing' },
  { key: 'camera_quality', label: 'Camera Image Quality Checks' },
  { key: 'active_tickets', label: 'Active Tickets' },
]

// ── Main component ───────────────────────────────────────────────────────────

const MainContent = ({ vehicles, isLoading, isRefreshing, error, onRefresh, onVehicleClick, projectFilter, latestCamTicketContent, onRefreshCamTickets, isRefreshingCamTickets, currentUserEmail }: Props) => {
  const [localGroup, setLocalGroup] = useState<ProjectGroup>('All')

  // ── Triage server state ──────────────────────────────────────────────────
  const [triageState, setTriageState] = useState<TriageState | null>(null)
  const [cycles, setCycles] = useState<TriageCycle[]>([])
  const [viewCycleId, setViewCycleId] = useState<number | null>(null)
  const [loadingTriage, setLoadingTriage] = useState(true)

  // ── Edit session state ───────────────────────────────────────────────────
  const [editingUser, setEditingUser] = useState<{ name: string; email: string } | null>(null)

  // ── Modals ───────────────────────────────────────────────────────────────
  const [showEditModal, setShowEditModal] = useState(false)
  const [editModalName, setEditModalName] = useState('')
  const [editModalEmail, setEditModalEmail] = useState('')
  const [editModalError, setEditModalError] = useState('')
  const [editModalLoading, setEditModalLoading] = useState(false)

  const [showStartTriageModal, setShowStartTriageModal] = useState(false)
  const [startTriageName, setStartTriageName] = useState('')
  const [startTriageEmail, setStartTriageEmail] = useState('')
  const [startTriageLoading, setStartTriageLoading] = useState(false)

  // ── Create ticket modal ──────────────────────────────────────────────────
  const [createTicketVehicle, setCreateTicketVehicle] = useState<{ id: string; run: string } | null>(null)
  const [createTicketIssue, setCreateTicketIssue] = useState('')
  const [createTicketDesc, setCreateTicketDesc] = useState('')
  const [createTicketDriveId, setCreateTicketDriveId] = useState('')
  const [createTicketLoading, setCreateTicketLoading] = useState(false)
  const [createTicketResult, setCreateTicketResult] = useState<{ key: string; url: string } | null>(null)
  const [createTicketError, setCreateTicketError] = useState('')

  const openCreateTicket = (v: { id: string; run: string }) => {
    setCreateTicketVehicle(v)
    setCreateTicketIssue('')
    setCreateTicketDesc('')
    setCreateTicketDriveId(v.run || '')
    setCreateTicketResult(null)
    setCreateTicketError('')
  }

  const submitCreateTicket = async () => {
    if (!createTicketVehicle || !createTicketIssue.trim() || !createTicketDesc.trim()) return
    setCreateTicketLoading(true)
    setCreateTicketError('')
    try {
      const res = await fetch('/api/triage/create-ticket', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          vehicle: createTicketVehicle.id,
          issue: createTicketIssue.trim(),
          description: createTicketDesc.trim(),
          drive_id: createTicketDriveId.trim(),
        }),
      })
      const data = await res.json()
      if (!res.ok) {
        setCreateTicketError(data.error || 'Failed to create ticket')
      } else {
        setCreateTicketResult(data)
        onRefreshCamTickets?.()
      }
    } catch {
      setCreateTicketError('Network error')
    } finally {
      setCreateTicketLoading(false)
    }
  }

  // ── View mode ────────────────────────────────────────────────────────────
  const [viewMode, setViewMode] = useState<'fleet' | 'triage'>('triage')

  // ── Complete triage ──────────────────────────────────────────────────────
  const [showCompleteModal, setShowCompleteModal] = useState(false)
  const [isCompleting, setIsCompleting] = useState(false)

  // ── Refresh tickets ──────────────────────────────────────────────────────
  const [isFetchingTickets, setIsFetchingTickets] = useState(false)

  const handleRefreshTickets = async () => {
    if (!triageState?.cycle) return
    setIsFetchingTickets(true)
    try {
      const res = await fetch('/api/triage/refresh-tickets', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ cycle_id: triageState.cycle.id }),
      })
      if (res.ok) {
        const data: TriageState = await res.json()
        // Only apply the two sections that refresh-tickets actually updates.
        // A full replacement would clobber any unsaved edits in "new" / "ongoing".
        setTriageState(prev => prev ? {
          ...prev,
          action_items: {
            ...prev.action_items,
            active_tickets: data.action_items?.active_tickets ?? prev.action_items?.active_tickets,
            camera_quality: data.action_items?.camera_quality ?? prev.action_items?.camera_quality,
          },
        } : data)
        // Also refresh the server-side camera-tickets cache bust so CQ badges
        // in Fleet View (which use latestCamTicketContent) reflect new tickets.
        onRefreshCamTickets?.()
      }
    } catch {}
    finally { setIsFetchingTickets(false) }
  }

  // ── Slack preview ────────────────────────────────────────────────────────
  const [isFetchingSlack, setIsFetchingSlack] = useState(false)
  const [slackPreview, setSlackPreview] = useState<string | null>(null)
  const [slackCopied, setSlackCopied] = useState(false)

  // ── App version ──────────────────────────────────────────────────────────
  const [appVersion, setAppVersion] = useState<string | null>(null)

  // ── Copy UX ──────────────────────────────────────────────────────────────
  const [copiedId, setCopiedId] = useState<string | null>(null)
  const handleCopy = (text: string) => {
    navigator.clipboard.writeText(text).catch(() => {
      const el = document.createElement('textarea')
      el.value = text
      document.body.appendChild(el)
      el.select()
      document.execCommand('copy')
      document.body.removeChild(el)
    })
    setCopiedId(text)
    setTimeout(() => setCopiedId(null), 1500)
  }

  // ── Load version ─────────────────────────────────────────────────────────
  useEffect(() => {
    fetch('/api/version').then(r => r.json()).then(d => setAppVersion(d.version)).catch(() => {})
  }, [])

  // ── Fetch triage state ───────────────────────────────────────────────────
  const fetchTriageState = useCallback(async () => {
    try {
      const url = viewCycleId
        ? `/api/triage/cycles/${viewCycleId}`
        : '/api/triage/current'
      const res = await fetch(url)
      if (res.ok) {
        const data: TriageState = await res.json()
        setTriageState(data)
      }
    } catch {}
  }, [viewCycleId])

  const fetchCycles = useCallback(async () => {
    try {
      const res = await fetch('/api/triage/cycles')
      if (res.ok) setCycles(await res.json())
    } catch {}
  }, [])

  // Initial load
  useEffect(() => {
    setLoadingTriage(true)
    Promise.all([fetchTriageState(), fetchCycles()]).finally(() => setLoadingTriage(false))
  }, [fetchTriageState, fetchCycles])

  // 30s polling — paused while in edit mode to prevent overwriting unsaved local changes.
  // The user's onChange updates are optimistic (local only); the server save fires on onBlur.
  // If the poll fires between a keystroke and the blur, it would clobber the in-flight edits.
  // When editing ends, handleDoneEditing calls fetchTriageState() directly to re-sync.
  useEffect(() => {
    if (editingUser) return
    const id = setInterval(fetchTriageState, 30_000)
    return () => clearInterval(id)
  }, [fetchTriageState, editingUser])

  // 20s heartbeat while editing
  useEffect(() => {
    if (!editingUser || !triageState?.cycle) return
    const id = setInterval(() => {
      fetch('/api/triage/edit-session/heartbeat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ cycle_id: triageState.cycle!.id, email: editingUser.email }),
      }).catch(() => {})
    }, 20_000)
    return () => clearInterval(id)
  }, [editingUser, triageState?.cycle?.id])

  // ── Derived state ────────────────────────────────────────────────────────
  const isViewingHistory = viewCycleId !== null
  const isEditMode = editingUser !== null && !isViewingHistory
  const isEditingCompletedItems = editingUser !== null && !!(triageState?.cycle?.completed_at)
  const activeEditor = triageState?.active_editor ?? null
  const anotherEditing = activeEditor && (!editingUser || activeEditor.email !== editingUser.email)

  const activeGroup: ProjectGroup = projectFilter
    ? (PROJECT_GROUPS.find(g => g !== 'All' && g === projectFilter) ?? 'All')
    : localGroup

  // Fleet view always uses live data; triage view uses snapshot for historical cycles
  const displayVehicles = (viewMode === 'triage' && isViewingHistory && triageState?.vehicles?.length)
    ? triageState.vehicles
    : vehicles

  const cycle = triageState?.cycle ?? null
  const annotations = triageState?.annotations ?? {}
  const actionItems = triageState?.action_items ?? {}

  const filteredVehicles = useMemo(
    () => displayVehicles.filter(v => matchesGroup(v.project, activeGroup)),
    [displayVehicles, activeGroup],
  )

  // ── Camera quality ticket map: vehicle ID → {key, url} ─────────────────
  // Lines in camera_quality have the format:
  //   "[ROG-103] - Camera Image Quality ... | https://jira-url"
  // Jira vehicle IDs are "ROG-103"; fleet vehicle IDs are "rog103" (no hyphen,
  // no uppercase). We normalize map keys by stripping the hyphen and lowercasing
  // so lookup via v.id (e.g. "rog103") works directly.
  const camTicketMap = useMemo(() => {
    const map = new Map<string, { key: string; url: string }>()
    const content = actionItems['camera_quality']?.content || latestCamTicketContent || ''
    for (const line of content.split('\n')) {
      const m = line.trim().match(/^\[([^\]]+)\].*\|\s*(https?:\/\/\S+)/)
      if (!m) continue
      // "ROG-103" → strip hyphen → "ROG103" → lowercase → "rog103"
      const vid = m[1].replace(/-/g, '').toLowerCase()
      const url = m[2]
      const keyMatch = url.match(/(VSTAB-\d+)/i)
      map.set(vid, { key: keyMatch ? keyMatch[1] : m[1], url })
    }
    return map
  }, [actionItems, latestCamTicketContent])

  // ── Completed triage summary (vehicle counts) ────────────────────────────
  const completedSummary = useMemo(() => {
    if (!cycle?.completed_at) return null
    const unhealthy = ['new', 'ongoing'].reduce(
      (acc, key) => acc + (actionItems[key]?.content ?? '').split('\n').filter(l => l.trim()).length,
      0,
    )
    const oos = displayVehicles.filter(v => v.inactive).length
    const healthy = Math.max(0, displayVehicles.length - oos - unhealthy)
    return { healthy, unhealthy, oos }
  }, [cycle?.completed_at, displayVehicles, actionItems])

  // ── Save annotation to server ────────────────────────────────────────────
  const saveAnnotation = useCallback(
    async (vehicleId: string, annot: Partial<TriageAnnotation>) => {
      if (!triageState?.cycle || !editingUser) return
      const existing = triageState.annotations[vehicleId] ?? defaultAnnot()
      const merged = { ...existing, ...annot }
      // Optimistic update
      setTriageState(prev => prev ? {
        ...prev,
        annotations: { ...prev.annotations, [vehicleId]: { ...merged, last_edited_by: editingUser.name, last_edited_at: new Date().toISOString() } },
      } : prev)
      fetch('/api/triage/annotations', {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          cycle_id: triageState.cycle.id,
          vehicle_id: vehicleId,
          ...merged,
          editor_name: editingUser.name,
          editor_email: editingUser.email,
        }),
      }).catch(() => {})
    },
    [triageState, editingUser],
  )

  // ── Save action item to server ───────────────────────────────────────────
  const saveActionItem = useCallback(
    async (section: string, content: string) => {
      if (!triageState?.cycle || !editingUser) return
      setTriageState(prev => prev ? {
        ...prev,
        action_items: {
          ...prev.action_items,
          [section]: { content, last_edited_by: editingUser.name, last_edited_at: new Date().toISOString() },
        },
      } : prev)
      fetch('/api/triage/action-items', {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          cycle_id: triageState.cycle.id,
          section,
          content,
          editor_name: editingUser.name,
          editor_email: editingUser.email,
        }),
      }).catch(() => {})
    },
    [triageState, editingUser],
  )

  // ── Edit session handlers ────────────────────────────────────────────────
  const handleStartEdit = async () => {
    if (!triageState?.cycle) return
    setEditModalError('')
    setEditModalLoading(true)
    try {
      const res = await fetch('/api/triage/edit-session/start', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          cycle_id: triageState.cycle.id,
          name: editModalName.trim(),
          email: editModalEmail.trim(),
        }),
      })
      if (res.status === 409) {
        const data = await res.json()
        const e = data.editor as ActiveEditor
        setEditModalError(`${e.name} (${e.email}) is currently editing.`)
        return
      }
      if (!res.ok) {
        const data = await res.json()
        setEditModalError(data.error ?? 'Failed to start edit session')
        return
      }
      setEditingUser({ name: editModalName.trim(), email: editModalEmail.trim() })
      setShowEditModal(false)
      setEditModalError('')
    } catch (err) {
      setEditModalError('Network error')
    } finally {
      setEditModalLoading(false)
    }
  }

  const handleDoneEditing = async () => {
    if (!triageState?.cycle || !editingUser) return
    await fetch('/api/triage/edit-session/done', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ cycle_id: triageState.cycle.id, email: editingUser.email }),
    }).catch(() => {})
    setEditingUser(null)
    await fetchTriageState()
  }

  // ── Start new triage ─────────────────────────────────────────────────────
  const handleStartTriage = async () => {
    setStartTriageLoading(true)
    try {
      const res = await fetch('/api/triage/start', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: startTriageName.trim(), email: startTriageEmail.trim() }),
      })
      if (!res.ok) {
        const data = await res.json()
        alert(data.error ?? 'Failed to start triage')
        return
      }
      const state: TriageState = await res.json()
      setTriageState(state)
      setViewCycleId(null)
      setEditingUser({ name: startTriageName.trim(), email: startTriageEmail.trim() })
      setShowStartTriageModal(false)
      await fetchCycles()
    } catch {
      alert('Network error')
    } finally {
      setStartTriageLoading(false)
    }
  }

  // ── Complete triage ──────────────────────────────────────────────────────
  const handleCompleteTriage = async () => {
    if (!triageState?.cycle) return
    setIsCompleting(true)
    try {
      const res = await fetch('/api/triage/complete', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ cycle_id: triageState.cycle.id }),
      })
      const data = await res.json()
      if (!res.ok) {
        alert(data.error ?? 'Failed to complete triage')
        return
      }
      setTriageState(data)
      setShowCompleteModal(false)
      if (editingUser) setEditingUser(null)
    } catch {
      alert('Network error')
    } finally {
      setIsCompleting(false)
    }
  }

  // ── Slack preview ────────────────────────────────────────────────────────
  const handleSlackPreview = async () => {
    if (!triageState?.cycle) return
    setIsFetchingSlack(true)
    try {
      const res = await fetch('/api/triage/post-slack', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ cycle_id: triageState.cycle.id }),
      })
      const data = await res.json()
      if (!res.ok) {
        alert(data.error ?? 'Failed to generate message')
        return
      }
      setSlackPreview(data.message)
      setSlackCopied(false)
    } catch {
      alert('Network error')
    } finally {
      setIsFetchingSlack(false)
    }
  }

  // ── Render ───────────────────────────────────────────────────────────────
  return (
    <>
      <div className="flex-1 overflow-auto">
        <div className="p-8">

          {/* Header */}
          <div className="mb-4 flex items-start justify-between gap-4">
            <div>
              <h1 className="text-3xl font-bold text-gray-900">
                {projectFilter ? `${projectFilter} Vehicles` : 'Onroad Fleet'}
              </h1>
              {cycle && (
                <p className="text-gray-500 mt-1 text-sm">
                  Triage started {fmtDate(cycle.created_at)} by {cycle.started_by_name}
                  {triageState?.last_updated && (
                    <> &middot; Last updated {fmtDate(triageState.last_updated)}</>
                  )}
                </p>
              )}
              {!cycle && !loadingTriage && (
                <p className="text-amber-600 mt-1 text-sm">No triage cycle started yet.</p>
              )}
            </div>

            <div className="flex items-center gap-2 mt-1 flex-shrink-0 flex-wrap justify-end">

              {/* View mode toggle */}
              <div className="flex items-center bg-gray-100 p-0.5 rounded-lg">
                <button
                  onClick={() => setViewMode('fleet')}
                  className={`px-3 py-1.5 rounded-md text-xs font-medium transition-colors ${
                    viewMode === 'fleet'
                      ? 'bg-white text-gray-900 shadow-sm'
                      : 'text-gray-500 hover:text-gray-700'
                  }`}
                >
                  Fleet View
                </button>
                <button
                  onClick={() => setViewMode('triage')}
                  className={`px-3 py-1.5 rounded-md text-xs font-medium transition-colors ${
                    viewMode === 'triage'
                      ? 'bg-white text-gray-900 shadow-sm'
                      : 'text-gray-500 hover:text-gray-700'
                  }`}
                >
                  Triage View
                </button>
              </div>

              {/* Cycle history dropdown — available in both views */}
              {cycles.length > 0 && (
                <select
                  value={viewCycleId ?? ''}
                  onChange={e => {
                    const val = e.target.value
                    setViewCycleId(val ? Number(val) : null)
                    if (val) setEditingUser(null)
                  }}
                  className="text-xs border border-gray-300 rounded-lg px-2 py-2 bg-white text-gray-700 focus:outline-none focus:ring-1 focus:ring-primary"
                >
                  <option value="">Current triage</option>
                  {cycles.filter(c => c.id !== triageState?.cycle?.id).map(c => (
                    <option key={c.id} value={c.id}>
                      Triage {(() => { const [y,m,d] = c.triage_date.split('-').map(Number); return new Date(y, m-1, d).toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: '2-digit' }) })()} ({c.started_by_name})
                    </option>
                  ))}
                </select>
              )}

              {/* Triage-only controls */}
              {viewMode === 'triage' && !isViewingHistory && (
                <button
                  onClick={() => setShowStartTriageModal(true)}
                  className="flex items-center gap-2 px-3 py-2 rounded-lg bg-white border border-gray-300 text-gray-700 font-medium text-sm hover:bg-gray-50 transition-colors"
                >
                  <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M12 4v16m8-8H4" />
                  </svg>
                  New Triage
                </button>
              )}

              {viewMode === 'triage' && !isViewingHistory && cycle && (
                isEditMode ? (
                  <button
                    onClick={handleDoneEditing}
                    className="flex items-center gap-2 px-3 py-2 rounded-lg bg-green-600 text-white font-medium text-sm hover:bg-green-700 transition-colors"
                  >
                    <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                      <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M5 13l4 4L19 7" />
                    </svg>
                    Done Editing
                  </button>
                ) : (
                  <button
                    onClick={() => { setEditModalError(''); setShowEditModal(true) }}
                    disabled={!!anotherEditing}
                    title={anotherEditing ? `${activeEditor?.name} is currently editing` : 'Edit triage annotations'}
                    className="flex items-center gap-2 px-3 py-2 rounded-lg bg-primary text-white font-medium text-sm hover:bg-primary-700 disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
                  >
                    <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                      <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M11 5H6a2 2 0 00-2 2v11a2 2 0 002 2h11a2 2 0 002-2v-5m-1.414-9.414a2 2 0 112.828 2.828L11.828 15H9v-2.828l8.586-8.586z" />
                    </svg>
                    Edit
                  </button>
                )
              )}

              {viewMode === 'triage' && !isViewingHistory && cycle && !cycle.completed_at && (
                <button
                  onClick={() => setShowCompleteModal(true)}
                  className="flex items-center gap-2 px-3 py-2 rounded-lg bg-white border border-gray-300 text-gray-700 font-medium text-sm hover:bg-gray-50 transition-colors"
                >
                  <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9 12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z" />
                  </svg>
                  Complete Triage
                </button>
              )}
              {viewMode === 'triage' && !isViewingHistory && cycle?.completed_at && (
                <span className="flex items-center gap-1.5 px-3 py-2 text-sm text-green-700 font-medium">
                  <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9 12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z" />
                  </svg>
                  Completed {fmtDate(cycle.completed_at)}
                </span>
              )}

              {viewMode === 'triage' && !isViewingHistory && cycle && (
                <button
                  onClick={handleSlackPreview}
                  disabled={isFetchingSlack}
                  className="flex items-center gap-2 px-3 py-2 rounded-lg bg-white border border-gray-300 text-gray-700 font-medium text-sm hover:bg-gray-50 disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
                >
                  {isFetchingSlack ? (
                    <div className="animate-spin h-4 w-4 border-2 border-gray-400 border-t-transparent rounded-full" />
                  ) : (
                    <svg className="w-4 h-4" fill="currentColor" viewBox="0 0 24 24">
                      <path d="M5.042 15.165a2.528 2.528 0 0 1-2.52 2.523A2.528 2.528 0 0 1 0 15.165a2.527 2.527 0 0 1 2.522-2.52h2.52v2.52zm1.271 0a2.527 2.527 0 0 1 2.521-2.52 2.527 2.527 0 0 1 2.521 2.52v6.313A2.528 2.528 0 0 1 8.834 24a2.528 2.528 0 0 1-2.521-2.522v-6.313zm2.521-10.123a2.528 2.528 0 0 1-2.521-2.52A2.528 2.528 0 0 1 8.834 0a2.528 2.528 0 0 1 2.521 2.522v2.52H8.834zm0 1.271a2.528 2.528 0 0 1 2.521 2.521 2.528 2.528 0 0 1-2.521 2.521H2.522A2.528 2.528 0 0 1 0 8.834a2.528 2.528 0 0 1 2.522-2.521h6.312zm10.122 2.521a2.528 2.528 0 0 1 2.522-2.521A2.528 2.528 0 0 1 24 8.834a2.528 2.528 0 0 1-2.522 2.521h-2.521V8.834zm-1.271 0a2.528 2.528 0 0 1-2.521 2.521 2.528 2.528 0 0 1-2.521-2.521V2.522A2.528 2.528 0 0 1 15.165 0a2.528 2.528 0 0 1 2.521 2.522v6.312zm-2.521 10.122a2.528 2.528 0 0 1 2.521 2.522A2.528 2.528 0 0 1 15.165 24a2.528 2.528 0 0 1-2.521-2.522v-2.521h2.521zm0-1.271a2.528 2.528 0 0 1-2.521-2.521 2.528 2.528 0 0 1 2.521-2.521h6.312A2.528 2.528 0 0 1 24 15.165a2.528 2.528 0 0 1-2.522 2.521h-6.312z"/>
                    </svg>
                  )}
                  Preview Slack
                </button>
              )}

              {/* Fleet-only: Fetch Newest Runs + Refresh Jira Tickets */}
              {viewMode === 'fleet' && (
                <>
                  <button
                    onClick={onRefreshCamTickets}
                    disabled={isRefreshingCamTickets}
                    title="Re-fetch active Jira tickets immediately (bypasses the 5-minute cache)"
                    className="flex items-center gap-2 px-3 py-2 rounded-lg bg-white border border-gray-300 text-gray-700 font-medium text-sm hover:bg-gray-50 disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
                  >
                    {isRefreshingCamTickets ? (
                      <>
                        <div className="animate-spin h-4 w-4 border-2 border-gray-400 border-t-transparent rounded-full" />
                        Refreshing...
                      </>
                    ) : (
                      <>
                        <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M7 7h.01M7 3h5c.512 0 1.024.195 1.414.586l7 7a2 2 0 010 2.828l-7 7a2 2 0 01-2.828 0l-7-7A2 2 0 013 12V7a2 2 0 014-4z" />
                        </svg>
                        Refresh Jira Tickets
                      </>
                    )}
                  </button>
                  <button
                    onClick={onRefresh}
                    disabled={isRefreshing || isLoading}
                    className="flex items-center gap-2 px-3 py-2 rounded-lg bg-white border border-gray-300 text-gray-700 font-medium text-sm hover:bg-gray-50 disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
                  >
                    {isRefreshing ? (
                      <>
                        <div className="animate-spin h-4 w-4 border-2 border-primary border-t-transparent rounded-full" />
                        Fetching...
                      </>
                    ) : (
                      <>
                        <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" />
                        </svg>
                        Fetch Newest Runs
                      </>
                    )}
                  </button>
                </>
              )}
            </div>
          </div>

          {/* Editor banner — triage view only */}
          {viewMode === 'triage' && isEditMode && (
            <div className="mb-4 px-4 py-2 bg-green-50 border border-green-200 rounded-lg text-sm text-green-800">
              Editing as <strong>{editingUser?.name}</strong> ({editingUser?.email}) — click "Done Editing" when finished.
            </div>
          )}
          {viewMode === 'triage' && anotherEditing && !isEditMode && (
            <div className="mb-4 px-4 py-2 bg-amber-50 border border-amber-200 rounded-lg text-sm text-amber-800 flex items-center justify-between gap-4">
              <span><strong>{activeEditor?.name}</strong> ({activeEditor?.email}) is currently editing.</span>
              <div className="flex gap-2 flex-shrink-0">
                <button
                  onClick={async () => {
                    if (!triageState?.cycle || !activeEditor) return
                    const res = await fetch('/api/triage/edit-session/start', {
                      method: 'POST',
                      headers: { 'Content-Type': 'application/json' },
                      body: JSON.stringify({ cycle_id: triageState.cycle.id, name: activeEditor.name, email: activeEditor.email }),
                    })
                    if (res.ok) setEditingUser({ name: activeEditor.name, email: activeEditor.email })
                  }}
                  className="px-3 py-1 rounded-md bg-amber-200 text-amber-900 text-xs font-medium hover:bg-amber-300 transition-colors"
                >
                  That's me — reclaim
                </button>
                <button
                  onClick={async () => {
                    if (!triageState?.cycle || !activeEditor) return
                    await fetch('/api/triage/edit-session/done', {
                      method: 'POST',
                      headers: { 'Content-Type': 'application/json' },
                      body: JSON.stringify({ cycle_id: triageState.cycle.id, email: activeEditor.email }),
                    })
                    await fetchTriageState()
                  }}
                  className="px-3 py-1 rounded-md bg-amber-100 text-amber-800 text-xs font-medium hover:bg-amber-200 border border-amber-300 transition-colors"
                >
                  Release lock
                </button>
              </div>
            </div>
          )}
          {isViewingHistory && (
            <div className="mb-4 px-4 py-2 bg-blue-50 border border-blue-200 rounded-lg text-sm text-blue-800">
              Viewing archived triage. <button onClick={() => setViewCycleId(null)} className="underline font-medium">Back to current</button>
            </div>
          )}

          {/* Completed Triage Report — styled read-only view for completed cycles */}
          {viewMode === 'triage' && cycle?.completed_at && (
            <div className="mb-6 space-y-4">
              {/* Vehicle Summary */}
              <div className="bg-white rounded-lg border border-gray-200 p-5">
                <h2 className="text-sm font-semibold text-gray-700 mb-4">Vehicle Summary</h2>
                <div className="flex gap-8">
                  <div className="flex flex-col items-center gap-1">
                    <span className="text-2xl font-bold text-green-600">{completedSummary?.healthy ?? 0}</span>
                    <div className="flex items-center gap-1.5">
                      <span className="w-2.5 h-2.5 rounded-full bg-green-500 flex-shrink-0" />
                      <span className="text-xs text-gray-500 whitespace-nowrap">Vehicles Healthy</span>
                    </div>
                  </div>
                  <div className="flex flex-col items-center gap-1">
                    <span className="text-2xl font-bold text-red-600">{completedSummary?.unhealthy ?? 0}</span>
                    <div className="flex items-center gap-1.5">
                      <span className="w-2.5 h-2.5 rounded-full bg-red-500 flex-shrink-0" />
                      <span className="text-xs text-gray-500 whitespace-nowrap">Vehicles Unhealthy</span>
                    </div>
                  </div>
                  <div className="flex flex-col items-center gap-1">
                    <span className="text-2xl font-bold text-gray-500">{completedSummary?.oos ?? 0}</span>
                    <div className="flex items-center gap-1.5">
                      <span className="w-2.5 h-2.5 rounded-full bg-gray-400 flex-shrink-0" />
                      <span className="text-xs text-gray-500 whitespace-nowrap">OOS / Inactive</span>
                    </div>
                  </div>
                </div>
              </div>

              {/* Action Items */}
              <div className="bg-white rounded-lg border border-gray-200 p-5">
                <div className="flex items-center justify-between mb-4">
                  <h2 className="text-sm font-semibold text-gray-700">Action Items</h2>
                  <div className="flex items-center gap-2">
                    <button
                      onClick={handleSlackPreview}
                      disabled={isFetchingSlack}
                      className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg bg-white border border-gray-300 text-gray-700 text-xs font-medium hover:bg-gray-50 disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
                    >
                      {isFetchingSlack ? (
                        <div className="animate-spin h-3.5 w-3.5 border-2 border-gray-400 border-t-transparent rounded-full" />
                      ) : (
                        <svg className="w-3.5 h-3.5" fill="currentColor" viewBox="0 0 24 24">
                          <path d="M5.042 15.165a2.528 2.528 0 0 1-2.52 2.523A2.528 2.528 0 0 1 0 15.165a2.527 2.527 0 0 1 2.522-2.52h2.52v2.52zm1.271 0a2.527 2.527 0 0 1 2.521-2.52 2.527 2.527 0 0 1 2.521 2.52v6.313A2.528 2.528 0 0 1 8.834 24a2.528 2.528 0 0 1-2.521-2.522v-6.313zm2.521-10.123a2.528 2.528 0 0 1-2.521-2.52A2.528 2.528 0 0 1 8.834 0a2.528 2.528 0 0 1 2.521 2.522v2.52H8.834zm0 1.271a2.528 2.528 0 0 1 2.521 2.521 2.528 2.528 0 0 1-2.521 2.521H2.522A2.528 2.528 0 0 1 0 8.834a2.528 2.528 0 0 1 2.522-2.521h6.312zm10.122 2.521a2.528 2.528 0 0 1 2.522-2.521A2.528 2.528 0 0 1 24 8.834a2.528 2.528 0 0 1-2.522 2.521h-2.521V8.834zm-1.271 0a2.528 2.528 0 0 1-2.521 2.521 2.528 2.528 0 0 1-2.521-2.521V2.522A2.528 2.528 0 0 1 15.165 0a2.528 2.528 0 0 1 2.521 2.522v6.312zm-2.521 10.122a2.528 2.528 0 0 1 2.521 2.522A2.528 2.528 0 0 1 15.165 24a2.528 2.528 0 0 1-2.521-2.522v-2.521h2.521zm0-1.271a2.528 2.528 0 0 1-2.521-2.521 2.528 2.528 0 0 1 2.521-2.521h6.312A2.528 2.528 0 0 1 24 15.165a2.528 2.528 0 0 1-2.522 2.521h-6.312z"/>
                        </svg>
                      )}
                      Preview Slack
                    </button>
                    {isEditingCompletedItems ? (
                      <button
                        onClick={handleDoneEditing}
                        className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg bg-green-600 text-white text-xs font-medium hover:bg-green-700 transition-colors"
                      >
                        <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M5 13l4 4L19 7" />
                        </svg>
                        Done Editing
                      </button>
                    ) : (
                      <button
                        onClick={() => { setEditModalError(''); setShowEditModal(true) }}
                        className="flex items-center gap-1.5 px-3 py-1.5 rounded-lg bg-white border border-gray-300 text-gray-700 text-xs font-medium hover:bg-gray-50 transition-colors"
                      >
                        <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M11 5H6a2 2 0 00-2 2v11a2 2 0 002 2h11a2 2 0 002-2v-5m-1.414-9.414a2 2 0 112.828 2.828L11.828 15H9v-2.828l8.586-8.586z" />
                        </svg>
                        Edit Action Items
                      </button>
                    )}
                  </div>
                </div>

                {isEditingCompletedItems ? (
                  <div className="grid grid-cols-2 gap-4">
                    {ACTION_SECTIONS.map(s => (
                      <div key={s.key}>
                        <div className="flex items-center gap-2 mb-1">
                          <label className="text-xs font-medium text-gray-600">{s.label}</label>
                          {s.key === 'active_tickets' && cycle && (
                            <button
                              onClick={handleRefreshTickets}
                              disabled={isFetchingTickets}
                              title="Re-fetch active VSTAB tickets from Jira"
                              className="inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-xs text-gray-500 hover:text-primary hover:bg-gray-100 disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
                            >
                              {isFetchingTickets ? (
                                <div className="animate-spin h-3 w-3 border border-gray-400 border-t-transparent rounded-full" />
                              ) : (
                                <svg className="w-3 h-3" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                                  <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" />
                                </svg>
                              )}
                              Fetch
                            </button>
                          )}
                        </div>
                        <textarea
                          value={actionItems[s.key]?.content ?? ''}
                          onChange={e => setTriageState(prev => prev ? {
                            ...prev,
                            action_items: {
                              ...prev.action_items,
                              [s.key]: { ...(prev.action_items[s.key] ?? { last_edited_by: '', last_edited_at: '' }), content: e.target.value },
                            },
                          } : prev)}
                          onBlur={e => saveActionItem(s.key, e.target.value)}
                          placeholder={`One item per line. Use "text | https://url" to add a Jira link.`}
                          rows={4}
                          className="w-full text-xs border border-gray-300 bg-white text-gray-700 rounded px-2 py-1.5 font-mono resize-y focus:outline-none focus:ring-1 focus:ring-primary"
                        />
                      </div>
                    ))}
                  </div>
                ) : ACTION_SECTIONS.some(s => (actionItems[s.key]?.content ?? '').trim()) ? (
                  <div className="space-y-5">
                    {ACTION_SECTIONS.map(s => {
                      const content = (actionItems[s.key]?.content ?? '').trim()
                      if (!content) return null
                      return (
                        <div key={s.key}>
                          <h3 className="text-xs font-semibold text-gray-500 uppercase tracking-wide mb-2">{s.label}</h3>
                          <ul className="space-y-1.5">
                            {content.split('\n').filter(l => l.trim()).map((line, i) => {
                              const parts = line.trim().split(' | ')
                              const text = parts[0].trim()
                              const url = parts[1]?.trim()
                              return (
                                <li key={i} className="flex items-start gap-2 text-sm text-gray-700">
                                  <span className="mt-[7px] w-1.5 h-1.5 rounded-full bg-gray-400 flex-shrink-0" />
                                  {url ? (
                                    <span>
                                      {text},{' '}
                                      <a href={url} target="_blank" rel="noopener noreferrer" className="text-primary hover:underline font-medium">
                                        Jira
                                      </a>
                                    </span>
                                  ) : (
                                    <span>{text}</span>
                                  )}
                                </li>
                              )
                            })}
                          </ul>
                        </div>
                      )
                    })}
                  </div>
                ) : (
                  <p className="text-xs text-gray-400">No action items recorded.</p>
                )}
              </div>
            </div>
          )}

          {/* Action Items — editable text areas for active (incomplete) triages */}
          {viewMode === 'triage' && !cycle?.completed_at && (cycle || isEditMode) && (
            <div className="mb-6 bg-white rounded-lg border border-gray-200 p-4">
              <h2 className="text-sm font-semibold text-gray-700 mb-3">Action Items</h2>
              <div className="grid grid-cols-2 gap-4">
                {ACTION_SECTIONS.map(s => (
                  <div key={s.key}>
                    <div className="flex items-center gap-2 mb-1">
                      <label className="text-xs font-medium text-gray-600">{s.label}</label>
                      {s.key === 'active_tickets' && cycle && (
                        <button
                          onClick={handleRefreshTickets}
                          disabled={isFetchingTickets}
                          title="Re-fetch active VSTAB tickets from Jira"
                          className="inline-flex items-center gap-1 px-1.5 py-0.5 rounded text-xs text-gray-500 hover:text-primary hover:bg-gray-100 disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
                        >
                          {isFetchingTickets ? (
                            <div className="animate-spin h-3 w-3 border border-gray-400 border-t-transparent rounded-full" />
                          ) : (
                            <svg className="w-3 h-3" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" />
                            </svg>
                          )}
                          Fetch
                        </button>
                      )}
                    </div>
                    <textarea
                      value={actionItems[s.key]?.content ?? ''}
                      onChange={e => {
                        if (!isEditMode) return
                        setTriageState(prev => prev ? {
                          ...prev,
                          action_items: {
                            ...prev.action_items,
                            [s.key]: { ...(prev.action_items[s.key] ?? { last_edited_by: '', last_edited_at: '' }), content: e.target.value },
                          },
                        } : prev)
                      }}
                      onBlur={e => isEditMode && saveActionItem(s.key, e.target.value)}
                      disabled={!isEditMode}
                      placeholder={isEditMode ? `One item per line. Use "text | https://url" to add a Jira link.` : '—'}
                      rows={4}
                      className={`w-full text-xs border rounded px-2 py-1.5 font-mono resize-y focus:outline-none focus:ring-1 focus:ring-primary ${
                        isEditMode
                          ? 'border-gray-300 bg-white text-gray-700'
                          : 'border-transparent bg-gray-50 text-gray-600 cursor-default'
                      }`}
                    />
                    {actionItems[s.key]?.last_edited_by && (
                      <p className="text-gray-400 text-xs mt-0.5">
                        Last edited by {actionItems[s.key].last_edited_by}
                      </p>
                    )}
                  </div>
                ))}
              </div>
            </div>
          )}

          {/* Project group filter */}
          {!projectFilter && (
            <div className="flex gap-1.5 mb-5">
              {PROJECT_GROUPS.map(group => {
                const count =
                  group === 'All'
                    ? displayVehicles.length
                    : displayVehicles.filter(v => matchesGroup(v.project, group)).length
                return (
                  <button
                    key={group}
                    onClick={() => setLocalGroup(group)}
                    className={`px-3 py-1.5 rounded-full text-xs font-medium transition-colors ${
                      localGroup === group
                        ? 'bg-primary text-white'
                        : 'bg-white border border-gray-300 text-gray-600 hover:bg-gray-50'
                    }`}
                  >
                    {group} <span className="opacity-70">({count})</span>
                  </button>
                )
              })}
            </div>
          )}

          {isLoading ? (
            <div className="flex items-center text-gray-500 gap-3">
              <div className="animate-spin h-5 w-5 border-2 border-primary border-t-transparent rounded-full" />
              Loading vehicles...
            </div>
          ) : error ? (
            <div className="text-red p-4 bg-red-50 rounded-lg">Error: {error}</div>
          ) : (
            <div className="bg-white rounded-lg border border-gray-300 overflow-hidden">
              <div className="overflow-auto max-h-[calc(100vh-16rem)]">
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b border-gray-200 bg-gray-50 sticky top-0 z-10">
                      <th className="text-left px-4 py-3 font-semibold text-gray-700">Vehicle</th>
                      <th className="text-left px-4 py-3 font-semibold text-gray-700">Project</th>
                      <th className="text-left px-4 py-3 font-semibold text-gray-700">Location</th>
                      <th className="text-left px-4 py-3 font-semibold text-gray-700 max-w-[140px]">Run</th>
                      <th className="text-left px-4 py-3 font-semibold text-gray-700 w-24">UUID</th>
                      <th className="text-left px-4 py-3 font-semibold text-gray-700">Snippets</th>
                      {CALIBRATION_COLUMNS.map(c => (
                        <th
                          key={c.key}
                          title={c.key.replace(/_/g, ' ')}
                          className="text-left px-2 py-3 font-semibold text-gray-700 whitespace-nowrap"
                        >
                          {c.label}
                        </th>
                      ))}
                      {viewMode === 'triage' && (
                        <>
                          <th className="text-left px-3 py-3 font-semibold text-gray-700 border-l border-gray-200 whitespace-nowrap">
                            Cam Quality
                          </th>
                          <th className="text-left px-3 py-3 font-semibold text-gray-700 whitespace-nowrap">
                            Drive Quality
                          </th>
                          <th className="text-left px-3 py-3 font-semibold text-gray-700">
                            Triage Comments
                          </th>
                          <th
                            className="text-center px-3 py-3 font-semibold text-gray-700 whitespace-nowrap"
                            title="Include this vehicle in the triage (for inactive / no-run vehicles)"
                          >
                            Include
                          </th>
                        </>
                      )}
                    </tr>
                  </thead>
                  <tbody>
                    {filteredVehicles.map((v, rowIdx) => {
                      const annot = annotations[v.id] ?? defaultAnnot()
                      const needsInclude = needsIncludeCheckbox(v)
                      return (
                        <tr
                          key={v.id}
                          className={`group border-b border-gray-100 hover:bg-blue-50/30 transition-colors ${
                            rowIdx % 2 === 0 ? 'bg-white' : 'bg-gray-50/60'
                          }`}
                        >
                          <td className="px-4 py-3">
                            <div className="flex items-center gap-1.5">
                              <button
                                onClick={() => onVehicleClick(v, isViewingHistory)}
                                className="font-semibold text-primary hover:underline text-left whitespace-nowrap"
                              >
                                {v.id}
                              </button>
                              {(() => {
                                const camTicket = camTicketMap.get(v.id.toLowerCase())
                                if (!camTicket) return null
                                return (
                                  <a
                                    href={camTicket.url || undefined}
                                    target="_blank"
                                    rel="noopener noreferrer"
                                    title={`Camera quality ticket: ${camTicket.key}`}
                                    className="inline-flex items-center gap-0.5 px-1.5 py-0.5 rounded text-xs font-medium bg-amber-50 text-amber-700 border border-amber-200 hover:bg-amber-100 transition-colors whitespace-nowrap"
                                    onClick={e => !camTicket.url && e.preventDefault()}
                                  >
                                    <svg className="w-3 h-3 flex-shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                                      <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M3 9a2 2 0 012-2h.93a2 2 0 001.664-.89l.812-1.22A2 2 0 0110.07 4h3.86a2 2 0 011.664.89l.812 1.22A2 2 0 0018.07 7H19a2 2 0 012 2v9a2 2 0 01-2 2H5a2 2 0 01-2-2V9z" />
                                      <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M15 13a3 3 0 11-6 0 3 3 0 016 0z" />
                                    </svg>
                                    CQ
                                  </a>
                                )
                              })()}
                              <button
                                onClick={() => openCreateTicket(v)}
                                title="Create VSTAB triage ticket for this vehicle"
                                className="opacity-0 group-hover:opacity-100 inline-flex items-center gap-0.5 px-1.5 py-0.5 rounded text-xs font-medium bg-blue-50 text-blue-600 border border-blue-200 hover:bg-blue-100 transition-all whitespace-nowrap"
                              >
                                <svg className="w-3 h-3" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                                  <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M12 4v16m8-8H4" />
                                </svg>
                                Ticket
                              </button>
                            </div>
                          </td>
                          <td className="px-4 py-3">
                            <ProjectBadge project={v.project} />
                          </td>
                          <td className="px-4 py-3">
                            <LocationBadge location={v.location} />
                          </td>
                          {v.stale ? (
                            <td colSpan={3} className="px-4 py-4">
                              {v.inactive ? (
                                <span className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md bg-gray-100 text-gray-500 text-xs font-medium">
                                  <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M18.364 18.364A9 9 0 005.636 5.636m12.728 12.728A9 9 0 115.636 5.636m12.728 12.728L5.636 5.636" />
                                  </svg>
                                  {v.stale_reason || 'not active in fleet'}
                                </span>
                              ) : (
                                <span className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md bg-amber-50 text-amber-700 text-xs font-medium">
                                  <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M12 9v2m0 4h.01M10.29 3.86L1.82 18a2 2 0 001.71 3h16.94a2 2 0 001.71-3L13.71 3.86a2 2 0 00-3.42 0z" />
                                  </svg>
                                  stale run — no new run noticed{v.stale_reason ? ` (${v.stale_reason})` : ''}
                                </span>
                              )}
                            </td>
                          ) : (
                            <>
                              <td className="px-4 py-4 max-w-[140px]">
                                <button
                                  onClick={() => handleCopy(v.run ?? '')}
                                  title={`Click to copy: ${v.run}`}
                                  className="font-mono text-xs text-gray-600 block truncate max-w-[140px] hover:text-primary transition-colors text-left"
                                >
                                  {copiedId === v.run ? (
                                    <span className="text-green-600 font-medium not-italic">copied</span>
                                  ) : (
                                    v.run
                                  )}
                                </button>
                              </td>
                              <td className="px-4 py-4 w-24">
                                <button
                                  onClick={() => handleCopy(v.run_uuid ?? '')}
                                  title={`Click to copy: ${v.run_uuid}`}
                                  className="font-mono text-xs text-gray-400 hover:text-primary transition-colors"
                                >
                                  {copiedId === v.run_uuid ? (
                                    <span className="text-green-600 font-medium not-italic">copied</span>
                                  ) : v.run_uuid ? (
                                    v.run_uuid.slice(0, 8) + '…'
                                  ) : (
                                    '—'
                                  )}
                                </button>
                              </td>
                              <td className="px-4 py-4">
                                <div className="flex flex-wrap gap-1.5">
                                  {v.snippets.map(s => (
                                    <a
                                      key={s.label}
                                      href={s.url}
                                      target="_blank"
                                      rel="noopener noreferrer"
                                      className="inline-flex items-center gap-1 px-2.5 py-1 rounded-md bg-primary-50 text-primary-600 hover:bg-primary-100 font-medium transition-colors text-xs whitespace-nowrap"
                                    >
                                      {s.label}
                                      <svg className="w-3 h-3" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                                        <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M10 6H6a2 2 0 00-2 2v10a2 2 0 002 2h10a2 2 0 002-2v-4M14 4h6m0 0v6m0-6L10 14" />
                                      </svg>
                                    </a>
                                  ))}
                                </div>
                              </td>
                            </>
                          )}
                          {CALIBRATION_COLUMNS.map(c => (
                            <td key={c.key} className="px-2 py-3">
                              <CalibrationCell calibrations={v.calibrations ?? []} camera={c.key} />
                            </td>
                          ))}
                          {/* Quality columns — triage view only */}
                          {viewMode === 'triage' && (
                            <>
                              <td className="px-3 py-3 border-l border-gray-100">
                                <QualityCell
                                  type="cam"
                                  quality={annot.cam_quality}
                                  note={annot.cam_note}
                                  disabled={!isEditMode}
                                  onSave={(q, n) => saveAnnotation(v.id, { cam_quality: q as CamQuality, cam_note: n })}
                                />
                              </td>
                              <td className="px-3 py-3">
                                <QualityCell
                                  type="drive"
                                  quality={annot.drive_quality}
                                  note={annot.drive_note}
                                  disabled={!isEditMode}
                                  onSave={(q, n) => saveAnnotation(v.id, { drive_quality: q as DriveQuality, drive_note: n })}
                                />
                              </td>
                              <td className="px-3 py-3 max-w-[180px]">
                                <input
                                  type="text"
                                  value={annot.triage_comments}
                                  onChange={e => {
                                    if (!isEditMode) return
                                    setTriageState(prev => prev ? {
                                      ...prev,
                                      annotations: {
                                        ...prev.annotations,
                                        [v.id]: { ...(prev.annotations[v.id] ?? defaultAnnot()), triage_comments: e.target.value },
                                      },
                                    } : prev)
                                  }}
                                  onBlur={e => isEditMode && saveAnnotation(v.id, { triage_comments: e.target.value })}
                                  disabled={!isEditMode}
                                  placeholder="—"
                                  className={`w-full text-xs placeholder-gray-300 focus:outline-none focus:ring-1 focus:ring-primary rounded px-1 py-0.5 transition-colors ${
                                    isEditMode
                                      ? 'bg-transparent text-gray-700 hover:bg-gray-50 focus:bg-white'
                                      : 'bg-transparent text-gray-600 cursor-default'
                                  }`}
                                />
                              </td>
                              <td className="px-3 py-3 text-center">
                                {needsInclude && (
                                  <input
                                    type="checkbox"
                                    checked={annot.included}
                                    onChange={e => isEditMode && saveAnnotation(v.id, { included: e.target.checked })}
                                    disabled={!isEditMode}
                                    title={annot.included ? 'Included in triage' : 'Excluded from triage'}
                                    className="w-4 h-4 text-primary rounded border-gray-300 focus:ring-primary cursor-pointer disabled:cursor-default"
                                  />
                                )}
                              </td>
                            </>
                          )}
                        </tr>
                      )
                    })}
                  </tbody>
                </table>
              </div>
            </div>
          )}
        </div>
        {appVersion && (
          <div className="px-8 pb-4 text-xs text-gray-400">{appVersion}</div>
        )}
      </div>

      {/* Edit modal */}
      {showEditModal && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/30"
          onClick={() => setShowEditModal(false)}
        >
          <div
            className="bg-white rounded-xl shadow-2xl w-full max-w-sm mx-4 p-6"
            onClick={e => e.stopPropagation()}
          >
            <h3 className="text-base font-semibold text-gray-900 mb-1">Start Editing</h3>
            <p className="text-sm text-gray-500 mb-4">Enter your name and email to edit this triage.</p>
            {editModalError && (
              <div className="mb-3 px-3 py-2 bg-red-50 border border-red-200 rounded text-sm text-red-700">
                {editModalError}
              </div>
            )}
            <div className="space-y-3 mb-4">
              <input
                type="text"
                value={editModalName}
                onChange={e => setEditModalName(e.target.value)}
                placeholder="Your name"
                className="w-full border border-gray-300 rounded-lg px-3 py-2 text-sm focus:outline-none focus:ring-1 focus:ring-primary"
                autoFocus
              />
              <input
                type="email"
                value={editModalEmail}
                onChange={e => setEditModalEmail(e.target.value)}
                onKeyDown={e => e.key === 'Enter' && handleStartEdit()}
                placeholder="your@email.com"
                className="w-full border border-gray-300 rounded-lg px-3 py-2 text-sm focus:outline-none focus:ring-1 focus:ring-primary"
              />
            </div>
            <div className="flex gap-2 justify-end">
              <button
                onClick={() => setShowEditModal(false)}
                className="px-4 py-2 rounded-lg text-sm text-gray-600 hover:bg-gray-100 transition-colors"
              >
                Cancel
              </button>
              <button
                onClick={handleStartEdit}
                disabled={!editModalName.trim() || !editModalEmail.trim() || editModalLoading}
                className="px-4 py-2 rounded-lg bg-primary text-white text-sm font-medium hover:bg-primary-700 disabled:opacity-50 transition-colors"
              >
                {editModalLoading ? 'Starting...' : 'Start Editing'}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Complete Triage confirmation modal */}
      {showCompleteModal && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/30"
          onClick={() => setShowCompleteModal(false)}
        >
          <div
            className="bg-white rounded-xl shadow-2xl w-full max-w-sm mx-4 p-6"
            onClick={e => e.stopPropagation()}
          >
            <h3 className="text-base font-semibold text-gray-900 mb-1">Complete Triage</h3>
            <p className="text-sm text-gray-600 mb-4">
              This will save a snapshot of the current vehicle data (runs, snippets, calibration dates)
              for this triage cycle. The snapshot will be shown when viewing this cycle in history.
              This cannot be undone.
            </p>
            <div className="flex gap-2 justify-end">
              <button
                onClick={() => setShowCompleteModal(false)}
                className="px-4 py-2 rounded-lg text-sm text-gray-600 hover:bg-gray-100 transition-colors"
              >
                Cancel
              </button>
              <button
                onClick={handleCompleteTriage}
                disabled={isCompleting}
                className="px-4 py-2 rounded-lg bg-green-600 text-white text-sm font-medium hover:bg-green-700 disabled:opacity-50 transition-colors"
              >
                {isCompleting ? 'Saving...' : 'Complete & Save Snapshot'}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Slack preview modal */}
      {slackPreview && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/30"
          onClick={() => setSlackPreview(null)}
        >
          <div
            className="bg-white rounded-xl shadow-2xl w-full max-w-2xl mx-4 p-6 max-h-[80vh] flex flex-col"
            onClick={e => e.stopPropagation()}
          >
            <div className="flex items-center justify-between mb-3">
              <h3 className="text-base font-semibold text-gray-900">Slack Message Preview</h3>
              <button onClick={() => setSlackPreview(null)} className="text-gray-400 hover:text-gray-600">
                <svg className="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M6 18L18 6M6 6l12 12" />
                </svg>
              </button>
            </div>
            <p className="text-sm text-gray-500 mb-3">
              Copy this message, then run <code className="bg-gray-100 px-1.5 py-0.5 rounded text-xs font-mono">/post-triage-slack</code> in Claude to post to #eng-sds-data-qa.
            </p>
            <pre className="flex-1 overflow-auto bg-gray-50 border border-gray-200 rounded-lg p-3 text-xs font-mono text-gray-700 whitespace-pre-wrap">
              {slackPreview}
            </pre>
            <div className="flex justify-end gap-2 mt-3">
              <button
                onClick={() => setSlackPreview(null)}
                className="px-4 py-2 rounded-lg text-sm text-gray-600 hover:bg-gray-100 transition-colors"
              >
                Close
              </button>
              <button
                onClick={() => {
                  navigator.clipboard.writeText(slackPreview).catch(() => {
                    const el = document.createElement('textarea')
                    el.value = slackPreview
                    document.body.appendChild(el)
                    el.select()
                    document.execCommand('copy')
                    document.body.removeChild(el)
                  })
                  setSlackCopied(true)
                  setTimeout(() => setSlackCopied(false), 2000)
                }}
                className="px-4 py-2 rounded-lg bg-primary text-white text-sm font-medium hover:bg-primary-700 transition-colors"
              >
                {slackCopied ? 'Copied!' : 'Copy'}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Start New Triage modal */}
      {showStartTriageModal && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/30"
          onClick={() => setShowStartTriageModal(false)}
        >
          <div
            className="bg-white rounded-xl shadow-2xl w-full max-w-sm mx-4 p-6"
            onClick={e => e.stopPropagation()}
          >
            <h3 className="text-base font-semibold text-gray-900 mb-1">Start New Triage Cycle</h3>
            <div className="mb-4 px-3 py-2 bg-amber-50 border border-amber-200 rounded text-sm text-amber-800">
              <strong>Warning:</strong> Starting a new triage will archive the current cycle and begin fresh. Previous annotations are preserved and can be viewed from the history dropdown.
            </div>
            <div className="space-y-3 mb-4">
              <input
                type="text"
                value={startTriageName}
                onChange={e => setStartTriageName(e.target.value)}
                placeholder="Your name"
                className="w-full border border-gray-300 rounded-lg px-3 py-2 text-sm focus:outline-none focus:ring-1 focus:ring-primary"
                autoFocus
              />
              <input
                type="email"
                value={startTriageEmail}
                onChange={e => setStartTriageEmail(e.target.value)}
                onKeyDown={e => e.key === 'Enter' && handleStartTriage()}
                placeholder="your@email.com"
                className="w-full border border-gray-300 rounded-lg px-3 py-2 text-sm focus:outline-none focus:ring-1 focus:ring-primary"
              />
            </div>
            <div className="flex gap-2 justify-end">
              <button
                onClick={() => setShowStartTriageModal(false)}
                className="px-4 py-2 rounded-lg text-sm text-gray-600 hover:bg-gray-100 transition-colors"
              >
                Cancel
              </button>
              <button
                onClick={handleStartTriage}
                disabled={!startTriageName.trim() || !startTriageEmail.trim() || startTriageLoading}
                className="px-4 py-2 rounded-lg bg-primary text-white text-sm font-medium hover:bg-primary-700 disabled:opacity-50 transition-colors"
              >
                {startTriageLoading ? 'Starting...' : 'Start New Triage'}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Create Ticket Modal */}
      {createTicketVehicle && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40">
          <div className="bg-white rounded-xl shadow-xl w-full max-w-md mx-4 p-6 flex flex-col gap-4">
            <div className="flex items-center justify-between">
              <h2 className="text-base font-semibold text-gray-900">Create Triage Ticket</h2>
              <button onClick={() => setCreateTicketVehicle(null)} className="text-gray-400 hover:text-gray-600 text-xl leading-none">&times;</button>
            </div>

            {createTicketResult ? (
              <div className="flex flex-col gap-3">
                <div className="flex items-center gap-2 text-green-700 bg-green-50 border border-green-200 rounded-lg px-3 py-2 text-sm">
                  <svg className="w-4 h-4 flex-shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M5 13l4 4L19 7" />
                  </svg>
                  Ticket created successfully
                </div>
                <a
                  href={createTicketResult.url}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-primary hover:underline font-medium text-sm"
                >
                  {createTicketResult.key} — open in Jira ↗
                </a>
                <button
                  onClick={() => setCreateTicketVehicle(null)}
                  className="mt-1 px-4 py-2 rounded-lg bg-gray-100 text-gray-700 text-sm font-medium hover:bg-gray-200 transition-colors self-end"
                >
                  Close
                </button>
              </div>
            ) : (
              <>
                <div className="flex flex-col gap-1">
                  <label className="text-xs font-medium text-gray-600">Vehicle</label>
                  <input
                    readOnly
                    value={createTicketVehicle.id}
                    className="px-3 py-2 border border-gray-200 rounded-lg text-sm bg-gray-50 text-gray-500"
                  />
                </div>
                <div className="flex flex-col gap-1">
                  <label className="text-xs font-medium text-gray-600">Reporter</label>
                  <input
                    readOnly
                    value={currentUserEmail || 'Loading...'}
                    className="px-3 py-2 border border-gray-200 rounded-lg text-sm bg-gray-50 text-gray-500"
                  />
                </div>
                <div className="flex flex-col gap-1">
                  <label className="text-xs font-medium text-gray-600">Issue Summary <span className="text-red-500">*</span></label>
                  <input
                    autoFocus
                    value={createTicketIssue}
                    onChange={e => setCreateTicketIssue(e.target.value)}
                    placeholder="e.g. Camera Image Quality [Front Center]"
                    className="px-3 py-2 border border-gray-300 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-primary/30"
                  />
                </div>
                <div className="flex flex-col gap-1">
                  <label className="text-xs font-medium text-gray-600">Description <span className="text-red-500">*</span></label>
                  <textarea
                    value={createTicketDesc}
                    onChange={e => setCreateTicketDesc(e.target.value)}
                    placeholder="Full observation text..."
                    rows={3}
                    className="px-3 py-2 border border-gray-300 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-primary/30 resize-none"
                  />
                </div>
                <div className="flex flex-col gap-1">
                  <label className="text-xs font-medium text-gray-600">Drive ID <span className="text-gray-400">(optional)</span></label>
                  <input
                    value={createTicketDriveId}
                    onChange={e => setCreateTicketDriveId(e.target.value)}
                    placeholder="e.g. rog112_20260830_132250"
                    className="px-3 py-2 border border-gray-300 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-primary/30"
                  />
                </div>
                {createTicketError && (
                  <p className="text-xs text-red-600">{createTicketError}</p>
                )}
                <div className="flex justify-end gap-2 pt-1">
                  <button
                    onClick={() => setCreateTicketVehicle(null)}
                    className="px-4 py-2 rounded-lg bg-gray-100 text-gray-700 text-sm font-medium hover:bg-gray-200 transition-colors"
                  >
                    Cancel
                  </button>
                  <button
                    onClick={submitCreateTicket}
                    disabled={createTicketLoading || !createTicketIssue.trim() || !createTicketDesc.trim()}
                    className="px-4 py-2 rounded-lg bg-primary text-white text-sm font-medium hover:bg-primary-700 disabled:opacity-50 transition-colors"
                  >
                    {createTicketLoading ? 'Creating...' : 'Create Ticket'}
                  </button>
                </div>
              </>
            )}
          </div>
        </div>
      )}
    </>
  )
}

export default MainContent
