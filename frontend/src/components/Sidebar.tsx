interface Props {
  activePage: string
  onPageChange: (page: string) => void
}

interface NavSection {
  header: string
  items: { id: string; label: string }[]
}

const Sidebar = ({ activePage, onPageChange }: Props) => {
  const sections: NavSection[] = [
    {
      header: 'Fleet',
      items: [
        { id: 'triage', label: 'Onroad Fleet' },
        { id: 'trucks', label: 'Trucks' },
      ],
    },
    {
      header: 'Programs',
      items: [
        { id: 'oasis', label: 'Oasis' },
        { id: 'neuron', label: 'Neuron' },
      ],
    },
    {
      header: 'Info',
      items: [{ id: 'about', label: 'About' }],
    },
  ]

  return (
    <div className="w-64 bg-white border-r border-gray-300 flex flex-col">
      <div className="p-6 border-b border-gray-300">
        <div className="flex items-center gap-3">
          {/* Calibration target icon */}
          <div className="flex-shrink-0 w-10 h-10 rounded-xl bg-primary flex items-center justify-center shadow-sm">
            <svg width="22" height="22" viewBox="0 0 22 22" fill="none" xmlns="http://www.w3.org/2000/svg">
              {/* Outer ring */}
              <circle cx="11" cy="11" r="9" stroke="white" strokeWidth="1.5" fill="none"/>
              {/* Inner ring */}
              <circle cx="11" cy="11" r="4.5" stroke="white" strokeWidth="1.5" fill="none"/>
              {/* Center dot */}
              <circle cx="11" cy="11" r="1.5" fill="white"/>
              {/* Crosshair lines */}
              <line x1="11" y1="1" x2="11" y2="5.5" stroke="white" strokeWidth="1.5" strokeLinecap="round"/>
              <line x1="11" y1="16.5" x2="11" y2="21" stroke="white" strokeWidth="1.5" strokeLinecap="round"/>
              <line x1="1" y1="11" x2="5.5" y2="11" stroke="white" strokeWidth="1.5" strokeLinecap="round"/>
              <line x1="16.5" y1="11" x2="21" y2="11" stroke="white" strokeWidth="1.5" strokeLinecap="round"/>
            </svg>
          </div>
          <div>
            <div className="text-base font-bold text-gray-900 leading-tight">Onroad</div>
            <div className="text-base font-bold text-primary leading-tight">QC</div>
          </div>
        </div>
      </div>

      <nav className="flex-1 p-4 space-y-4">
        {sections.map((section) => (
          <div key={section.header}>
            <div className="px-4 pb-1 text-xs font-semibold text-gray-400 uppercase tracking-wider">
              {section.header}
            </div>
            <ul className="space-y-0.5">
              {section.items.map((page) => (
                <li key={page.id}>
                  <button
                    onClick={() => onPageChange(page.id)}
                    className={`w-full text-left px-4 py-2 rounded-lg transition-colors ${
                      activePage === page.id
                        ? 'bg-primary text-white'
                        : 'text-gray-700 hover:bg-gray-100'
                    }`}
                  >
                    {page.label}
                  </button>
                </li>
              ))}
            </ul>
          </div>
        ))}
      </nav>

      <div className="p-4 border-t border-gray-300">
        <div className="text-xs text-gray-500">Version 1.0.0</div>
      </div>
    </div>
  )
}

export default Sidebar
