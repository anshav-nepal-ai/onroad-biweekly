import { useState, useEffect } from 'react'
import { Vehicle, RecentRun, CameraCalibration } from '../App'

interface Props {
  vehicle: Vehicle
  allVehicles?: Vehicle[]
  backLabel?: string
  onBack: () => void
  isHistorical?: boolean
}

const cameraLabel = (name: string) =>
  name.replace(/_/g, ' ').replace(/\b\w/g, c => c.toUpperCase())

const vehicleJiraID = (id: string) => {
  const m = id.match(/^([a-z]+)(\d+)$/)
  return m ? `${m[1].toUpperCase()}-${m[2]}` : id.toUpperCase()
}

const projectStyle = (project?: string): string => {
  const p = project || 'Neuron Gen-1'
  if (p.startsWith('Oasis'))    return 'bg-blue-50 text-blue-700'
  if (p.startsWith('Robotaxi')) return 'bg-purple-50 text-purple-700'
  if (p.endsWith('Gen-2'))      return 'bg-green-50 text-green-700'
  return 'bg-gray-100 text-gray-600'
}

const VehicleDetail = ({ vehicle: v, backLabel = 'Back', onBack, isHistorical = false }: Props) => {
  const calibrations = v.calibrations ?? []

  // Create ticket modal
  const [showModal, setShowModal] = useState(false)
  const [issue, setIssue] = useState('')
  const [desc, setDesc] = useState('')
  const [driveId, setDriveId] = useState('')
  const [loading, setLoading] = useState(false)
  const [result, setResult] = useState<{ key: string; url: string } | null>(null)
  const [modalError, setModalError] = useState('')
  const [userEmail, setUserEmail] = useState('')

  useEffect(() => {
    fetch('/api/me').then(r => r.json()).then(d => setUserEmail(d.email ?? '')).catch(() => {})
  }, [])

  const openModal = () => {
    setIssue('')
    setDesc('')
    setDriveId(v.run || '')
    setResult(null)
    setModalError('')
    setShowModal(true)
  }

  const submitTicket = async () => {
    if (!issue.trim() || !desc.trim()) return
    setLoading(true)
    setModalError('')
    try {
      const res = await fetch('/api/triage/create-ticket', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ vehicle: v.id, issue: issue.trim(), description: desc.trim(), drive_id: driveId.trim() }),
      })
      const data = await res.json()
      if (!res.ok) setModalError(data.error || 'Failed to create ticket')
      else setResult(data)
    } catch {
      setModalError('Network error')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="flex-1 overflow-auto">
      <div className="p-8 max-w-6xl mx-auto">

        {/* Header */}
        <div className="mb-6">
          <button
            onClick={onBack}
            className="flex items-center gap-1.5 text-sm text-gray-500 hover:text-gray-800 mb-4 transition-colors"
          >
            <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M15 19l-7-7 7-7" />
            </svg>
            {backLabel}
          </button>

          <div className="flex items-center gap-4">
            <h1 className="text-3xl font-bold text-gray-900">{v.id}</h1>
            <span className={`inline-flex px-3 py-1 rounded-md text-sm font-medium ${projectStyle(v.project)}`}>
              {v.project || 'Neuron Gen-1'}
            </span>
            {v.location && (
              <span className="inline-flex px-3 py-1 rounded-md text-sm font-medium bg-slate-100 text-slate-600">
                {v.location}
              </span>
            )}
            {v.inactive ? (
              <span className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md bg-gray-100 text-gray-500 text-xs font-medium">
                <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M18.364 18.364A9 9 0 005.636 5.636m12.728 12.728A9 9 0 115.636 5.636m12.728 12.728L5.636 5.636" />
                </svg>
                inactive / OOS{v.stale_reason ? ` — ${v.stale_reason}` : ''}
              </span>
            ) : v.stale ? (
              <span className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md bg-amber-50 text-amber-700 text-xs font-medium">
                stale{v.stale_reason ? ` — ${v.stale_reason}` : ''}
              </span>
            ) : null}
          </div>
          {!v.stale && (
            <p className="text-gray-500 text-sm mt-1 font-mono">{v.run} · {v.run_uuid}</p>
          )}
        </div>

        {/* Jira Actions */}
        <div className="bg-white rounded-lg border border-gray-200 p-4 mb-6">
          <div className="flex items-center gap-3 flex-wrap">
            <span className="text-sm font-semibold text-gray-700 mr-1">Jira</span>
            <button
              onClick={openModal}
              className="inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md bg-primary text-white text-xs font-medium hover:bg-primary-700 transition-colors"
            >
              <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M12 4v16m8-8H4" />
              </svg>
              Create Ticket
            </button>
          </div>
        </div>

        {/* Recent run snippets — only in live/active context, not historical completed triage */}
        {!isHistorical && ((v.recent_runs || []).length > 0 ? (
          <div className="bg-white rounded-lg border border-gray-200 p-6 mb-6">
            <h2 className="text-base font-semibold text-gray-900 mb-1">
              Recent Run Snippets
              {v.inactive ? (
                <span className="ml-2 text-xs font-normal text-gray-500">last known runs — vehicle is inactive / OOS</span>
              ) : v.stale ? (
                <span className="ml-2 text-xs font-normal text-amber-600">last known runs — vehicle is stale</span>
              ) : null}
            </h2>
            <p className="text-xs text-gray-400 mb-4">Last {(v.recent_runs || []).length} run{(v.recent_runs || []).length !== 1 ? 's' : ''}, newest first</p>
            <div className="space-y-4">
              {(v.recent_runs || []).map((r: RecentRun, idx: number) => (
                <div key={r.run_uuid} className={idx > 0 ? 'pt-4 border-t border-gray-100' : ''}>
                  <div className="flex items-center gap-3 mb-2">
                    <span className="font-mono text-sm font-semibold text-gray-800">{r.run}</span>
                    <span className="text-xs text-gray-400">{r.run_date}</span>
                    {idx === 0 && (
                      <span className="text-xs bg-primary-50 text-primary-600 px-2 py-0.5 rounded-full font-medium">latest</span>
                    )}
                  </div>
                  <div className="flex flex-wrap gap-3">
                    {(r.snippets || []).map(s => (
                      <a
                        key={s.label}
                        href={s.url}
                        target="_blank"
                        rel="noopener noreferrer"
                        className="inline-flex items-center gap-1.5 px-4 py-2 rounded-lg bg-primary-50 text-primary-600 hover:bg-primary-100 font-medium transition-colors text-sm"
                      >
                        {s.label}
                        <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M10 6H6a2 2 0 00-2 2v10a2 2 0 002 2h10a2 2 0 002-2v-4M14 4h6m0 0v6m0-6L10 14" />
                        </svg>
                      </a>
                    ))}
                  </div>
                </div>
              ))}
            </div>
          </div>
        ) : v.inactive ? (
          <div className="bg-white rounded-lg border border-gray-200 p-6 mb-6">
            <h2 className="text-base font-semibold text-gray-900 mb-1">Recent Run Snippets</h2>
            <p className="text-sm text-gray-400">No recent run snippets available — vehicle is inactive / out of service.</p>
          </div>
        ) : null)}

        {/* Calibration */}
        <h2 className="text-base font-semibold text-gray-900 mb-3">
          Calibration
          {calibrations.filter(c => c.stale).length > 0 && (
            <span className="ml-2 text-sm font-normal text-red-600">
              {calibrations.filter(c => c.stale).length} overdue
            </span>
          )}
        </h2>
        {calibrations.length > 0 ? (
          <div className="bg-white rounded-lg border border-gray-300 overflow-hidden">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-gray-200 bg-gray-50">
                  <th className="text-left px-5 py-3 font-semibold text-gray-700">Camera</th>
                  <th className="text-left px-5 py-3 font-semibold text-gray-700">Last Calibrated</th>
                  <th className="text-right px-5 py-3 font-semibold text-gray-700">Days Since</th>
                  <th className="text-left px-5 py-3 font-semibold text-gray-700">Status</th>
                </tr>
              </thead>
              <tbody>
                {calibrations.map((c: CameraCalibration) => (
                  <tr key={c.camera} className={`border-b border-gray-100 ${c.stale ? 'bg-red-50/30' : ''}`}>
                    <td className="px-5 py-4 font-medium text-gray-800">{cameraLabel(c.camera)}</td>
                    <td className="px-5 py-4 font-mono text-gray-600">{c.last_calibrated}</td>
                    <td className="px-5 py-4 text-right font-mono text-gray-600">{c.days_since}d</td>
                    <td className="px-5 py-4">
                      {c.stale ? (
                        <span className="inline-flex px-2.5 py-1 rounded-md text-xs font-medium bg-red-100 text-red-700">
                          Overdue
                        </span>
                      ) : (
                        <span className="inline-flex px-2.5 py-1 rounded-md text-xs font-medium bg-green-50 text-green-700">
                          OK
                        </span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <div className="bg-white rounded-lg border border-gray-200 p-6 text-sm text-gray-400">
            No calibration data available for this vehicle.
          </div>
        )}
      </div>

      {/* Create Ticket Modal */}
      {showModal && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40">
          <div className="bg-white rounded-xl shadow-xl w-full max-w-md mx-4 p-6 flex flex-col gap-4">
            <div className="flex items-center justify-between">
              <h2 className="text-base font-semibold text-gray-900">Create Triage Ticket</h2>
              <button onClick={() => setShowModal(false)} className="text-gray-400 hover:text-gray-600 text-xl leading-none">&times;</button>
            </div>

            {result ? (
              <div className="flex flex-col gap-3">
                <div className="flex items-center gap-2 text-green-700 bg-green-50 border border-green-200 rounded-lg px-3 py-2 text-sm">
                  <svg className="w-4 h-4 flex-shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M5 13l4 4L19 7" />
                  </svg>
                  Ticket created successfully
                </div>
                <a href={result.url} target="_blank" rel="noopener noreferrer" className="text-primary hover:underline font-medium text-sm">
                  {result.key} — open in Jira ↗
                </a>
                <button onClick={() => setShowModal(false)} className="mt-1 px-4 py-2 rounded-lg bg-gray-100 text-gray-700 text-sm font-medium hover:bg-gray-200 transition-colors self-end">
                  Close
                </button>
              </div>
            ) : (
              <>
                <div className="flex flex-col gap-1">
                  <label className="text-xs font-medium text-gray-600">Vehicle</label>
                  <input readOnly value={vehicleJiraID(v.id)} className="px-3 py-2 border border-gray-200 rounded-lg text-sm bg-gray-50 text-gray-500" />
                </div>
                <div className="flex flex-col gap-1">
                  <label className="text-xs font-medium text-gray-600">Reporter</label>
                  <input readOnly value={userEmail || 'Loading...'} className="px-3 py-2 border border-gray-200 rounded-lg text-sm bg-gray-50 text-gray-500" />
                </div>
                <div className="flex flex-col gap-1">
                  <label className="text-xs font-medium text-gray-600">Issue Summary <span className="text-red-500">*</span></label>
                  <input autoFocus value={issue} onChange={e => setIssue(e.target.value)} placeholder="e.g. Camera Image Quality [Front Center]" className="px-3 py-2 border border-gray-300 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-primary/30" />
                </div>
                <div className="flex flex-col gap-1">
                  <label className="text-xs font-medium text-gray-600">Description <span className="text-red-500">*</span></label>
                  <textarea value={desc} onChange={e => setDesc(e.target.value)} placeholder="Full observation text..." rows={3} className="px-3 py-2 border border-gray-300 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-primary/30 resize-none" />
                </div>
                <div className="flex flex-col gap-1">
                  <label className="text-xs font-medium text-gray-600">Drive ID <span className="text-gray-400">(optional)</span></label>
                  <input value={driveId} onChange={e => setDriveId(e.target.value)} placeholder="e.g. rog112_20260830_132250" className="px-3 py-2 border border-gray-300 rounded-lg text-sm focus:outline-none focus:ring-2 focus:ring-primary/30" />
                </div>
                {modalError && <p className="text-xs text-red-600">{modalError}</p>}
                <div className="flex justify-end gap-2 pt-1">
                  <button onClick={() => setShowModal(false)} className="px-4 py-2 rounded-lg bg-gray-100 text-gray-700 text-sm font-medium hover:bg-gray-200 transition-colors">Cancel</button>
                  <button onClick={submitTicket} disabled={loading || !issue.trim() || !desc.trim()} className="px-4 py-2 rounded-lg bg-primary text-white text-sm font-medium hover:bg-primary-700 disabled:opacity-50 transition-colors">
                    {loading ? 'Creating...' : 'Create Ticket'}
                  </button>
                </div>
              </>
            )}
          </div>
        </div>
      )}
    </div>
  )
}

export default VehicleDetail
