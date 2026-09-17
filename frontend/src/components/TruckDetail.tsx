import { Truck, TruckCamera } from '../App'

interface Props {
  truck: Truck
  onBack: () => void
}

const cameraLabel = (name: string) =>
  name.replace(/_/g, ' ').replace(/\b\w/g, c => c.toUpperCase())

const TruckDetail = ({ truck: t, onBack }: Props) => {
  const at360 = t.cameras.filter(c => c.group === 'AT360')
  const qt128 = t.cameras.filter(c => c.group === 'QT128')

  return (
    <div className="flex-1 overflow-auto">
      <div className="p-8 max-w-6xl mx-auto">
        <div className="mb-6">
          <button
            onClick={onBack}
            className="flex items-center gap-1.5 text-sm text-gray-500 hover:text-gray-800 mb-4 transition-colors"
          >
            <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M15 19l-7-7 7-7" />
            </svg>
            Back to Trucks
          </button>

          <div className="flex items-center gap-4 flex-wrap">
            <h1 className="text-2xl font-bold font-mono text-gray-900">{t.uuid.slice(0, 8)}…</h1>
            <span className="inline-flex px-3 py-1 rounded-md text-sm font-medium bg-slate-100 text-slate-600">
              Isuzu CYJ77D-WX-D
            </span>
            {t.stale ? (
              <span className="inline-flex px-3 py-1 rounded-md text-sm font-medium bg-amber-50 text-amber-700">stale</span>
            ) : (
              <span className="inline-flex px-3 py-1 rounded-md text-sm font-medium bg-green-50 text-green-700">fresh</span>
            )}
          </div>
          <p className="text-sm text-gray-500 mt-1 font-mono">{t.uuid}</p>
          <p className="text-sm text-gray-500 mt-0.5">{t.date}</p>
        </div>

        {at360.length > 0 && (
          <CameraSection title="Cameras — AT360" cameras={at360} />
        )}
        {qt128.length > 0 && (
          <CameraSection title="Lidar — QT128" cameras={qt128} />
        )}

        {t.cameras.length === 0 && (
          <div className="bg-white rounded-lg border border-gray-200 p-6 text-sm text-gray-400">
            No calibration data available for this run.
          </div>
        )}
      </div>
    </div>
  )
}

const CameraSection = ({ title, cameras }: { title: string; cameras: TruckCamera[] }) => (
  <div className="mb-8">
    <h2 className="text-base font-semibold text-gray-900 mb-3">{title}</h2>
    <div className="space-y-4">
      {cameras.map(cam => (
        <div key={cam.name} className="bg-white rounded-lg border border-gray-200 p-4">
          <p className="text-sm font-semibold text-gray-700 mb-3">{cameraLabel(cam.name)}</p>
          <div className="flex flex-wrap gap-3">
            {cam.snippets.map(s => (
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
)

export default TruckDetail
