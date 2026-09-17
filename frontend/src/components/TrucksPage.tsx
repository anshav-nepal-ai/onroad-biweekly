import { Truck } from '../App'

interface Props {
  trucks: Truck[]
  isLoading: boolean
  isRefreshing: boolean
  error: string | null
  onRefresh: () => void
  onTruckClick: (t: Truck) => void
}

const TrucksPage = ({ trucks, isLoading, isRefreshing, error, onRefresh, onTruckClick }: Props) => {
  const stale = trucks.filter(t => t.stale)
  const fresh = trucks.filter(t => !t.stale)

  return (
    <div className="flex-1 overflow-auto">
      <div className="p-8 max-w-6xl mx-auto">
        <div className="mb-6 flex items-center justify-between">
          <div>
            <h1 className="text-2xl font-bold text-gray-900">Trucks</h1>
            <p className="text-sm text-gray-500 mt-0.5">
              Frontier cluster calibration runs — Isuzu CYJ77D-WX-D
            </p>
          </div>
          <button
            onClick={onRefresh}
            disabled={isRefreshing}
            className="inline-flex items-center gap-2 px-4 py-2 rounded-lg bg-primary text-white text-sm font-medium hover:bg-primary-700 disabled:opacity-60 transition-colors"
          >
            {isRefreshing ? (
              <>
                <svg className="w-4 h-4 animate-spin" fill="none" viewBox="0 0 24 24">
                  <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
                  <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8v8z" />
                </svg>
                Refreshing…
              </>
            ) : (
              <>
                <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" />
                </svg>
                Refresh Trucks
              </>
            )}
          </button>
        </div>

        {error && (
          <div className="mb-4 px-4 py-3 rounded-lg bg-red-50 border border-red-200 text-sm text-red-700">{error}</div>
        )}

        {isLoading ? (
          <div className="flex items-center justify-center h-48 text-gray-400 text-sm">Loading trucks…</div>
        ) : trucks.length === 0 ? (
          <div className="flex flex-col items-center justify-center h-48 gap-3 text-gray-400">
            <p className="text-sm">No calibration runs found.</p>
            <button onClick={onRefresh} className="text-sm text-primary hover:underline">Refresh to fetch from S3</button>
          </div>
        ) : (
          <>
            {fresh.length > 0 && (
              <section className="mb-8">
                <h2 className="text-sm font-semibold text-gray-500 uppercase tracking-wider mb-3">
                  Recent ({fresh.length})
                </h2>
                <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
                  {fresh.map(t => <TruckCard key={t.uuid} truck={t} onClick={() => onTruckClick(t)} />)}
                </div>
              </section>
            )}
            {stale.length > 0 && (
              <section>
                <h2 className="text-sm font-semibold text-gray-500 uppercase tracking-wider mb-3">
                  Stale — older than 7 days ({stale.length})
                </h2>
                <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
                  {stale.map(t => <TruckCard key={t.uuid} truck={t} onClick={() => onTruckClick(t)} />)}
                </div>
              </section>
            )}
          </>
        )}
      </div>
    </div>
  )
}

const TruckCard = ({ truck: t, onClick }: { truck: Truck; onClick: () => void }) => {
  const at360Count = t.cameras.filter(c => c.group === 'AT360').length
  const qt128Count = t.cameras.filter(c => c.group === 'QT128').length
  const shortUUID = t.uuid.slice(0, 8)

  return (
    <button
      onClick={onClick}
      className="text-left bg-white rounded-lg border border-gray-200 p-4 hover:border-primary/40 hover:shadow-sm transition-all"
    >
      <div className="flex items-start justify-between gap-2 mb-2">
        <span className="font-mono text-sm font-semibold text-gray-800">{shortUUID}…</span>
        {t.stale ? (
          <span className="inline-flex px-2 py-0.5 rounded-md text-xs font-medium bg-amber-50 text-amber-700 flex-shrink-0">stale</span>
        ) : (
          <span className="inline-flex px-2 py-0.5 rounded-md text-xs font-medium bg-green-50 text-green-700 flex-shrink-0">fresh</span>
        )}
      </div>
      <p className="text-xs text-gray-500 mb-3">{t.date}</p>
      <div className="flex gap-3 text-xs text-gray-500">
        {at360Count > 0 && <span className="bg-gray-100 px-2 py-0.5 rounded">AT360 ×{at360Count}</span>}
        {qt128Count > 0 && <span className="bg-gray-100 px-2 py-0.5 rounded">QT128 ×{qt128Count}</span>}
      </div>
    </button>
  )
}

export default TrucksPage
