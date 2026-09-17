import { useState, useEffect } from 'react'
import Sidebar from './components/Sidebar'
import MainContent from './components/MainContent'
import VehicleDetail from './components/VehicleDetail'
import AboutPage from './components/AboutPage'
import TrucksPage from './components/TrucksPage'
import TruckDetail from './components/TruckDetail'

export interface Snippet {
  label: string
  url: string
}

export interface RecentRun {
  run: string
  run_uuid: string
  run_date: string
  snippets: Snippet[]
}

export interface FilterEntry {
  filter_name: string
  fail_seconds: number
  fail_pct: number
}

export interface QualityRun {
  run_id: string
  run_uuid: string
  run_minutes: number
  filters: FilterEntry[]
}

export interface CameraCalibration {
  camera: string
  last_calibrated: string
  days_since: number
  stale: boolean
}

export interface TruckCamera {
  name: string
  group: string
  snippets: Snippet[]
}

export interface Truck {
  uuid: string
  date: string
  stale: boolean
  cameras: TruckCamera[]
}

export interface Vehicle {
  id: string
  project?: string
  location?: string
  run: string
  run_uuid?: string
  cam_gen: string
  snippets: Snippet[]
  recent_runs: RecentRun[]
  stale: boolean
  stale_reason?: string
  inactive?: boolean
  quality_runs?: QualityRun[]
  calibrations: CameraCalibration[]
}

const PAGE_LABELS: Record<string, string> = {
  triage: 'Onroad Fleet',
  oasis: 'Oasis Vehicles',
  neuron: 'Neuron Vehicles',
  trucks: 'Trucks',
  about: 'About',
}

function App() {
  const [vehicles, setVehicles] = useState<Vehicle[]>([])
  const [isLoading, setIsLoading] = useState(true)
  const [isRefreshing, setIsRefreshing] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [selectedVehicle, setSelectedVehicle] = useState<Vehicle | null>(null)
  const [isHistoricalVehicle, setIsHistoricalVehicle] = useState(false)
  const [activePage, setActivePage] = useState('triage')
  const [latestCamTicketContent, setLatestCamTicketContent] = useState('')
  const [isRefreshingCamTickets, setIsRefreshingCamTickets] = useState(false)
  const [currentUserEmail, setCurrentUserEmail] = useState('')

  const [trucks, setTrucks] = useState<Truck[]>([])
  const [isLoadingTrucks, setIsLoadingTrucks] = useState(false)
  const [isRefreshingTrucks, setIsRefreshingTrucks] = useState(false)
  const [trucksError, setTrucksError] = useState<string | null>(null)
  const [selectedTruck, setSelectedTruck] = useState<Truck | null>(null)

  const fetchVehicles = () => {
    setIsLoading(true)
    setError(null)
    fetch('/api/vehicles')
      .then(r => r.json())
      .then((data: Vehicle[]) => { setVehicles(data); setIsLoading(false) })
      .catch(err => { setError(err.message); setIsLoading(false) })
  }

  const fetchNewestRuns = () => {
    setIsRefreshing(true)
    setError(null)
    fetch('/api/refresh', { method: 'POST' })
      .then(r => {
        if (!r.ok) return r.json().then(e => Promise.reject(new Error(e.error || 'Refresh failed')))
        return r.json()
      })
      .then((data: Vehicle[]) => { setVehicles(data); setIsRefreshing(false) })
      .catch(err => { setError(err.message); setIsRefreshing(false) })
  }

  const fetchTrucks = () => {
    setIsLoadingTrucks(true)
    setTrucksError(null)
    fetch('/api/trucks')
      .then(r => r.json())
      .then((data: Truck[]) => { setTrucks(data ?? []); setIsLoadingTrucks(false) })
      .catch(err => { setTrucksError(err.message); setIsLoadingTrucks(false) })
  }

  const refreshTrucks = () => {
    setIsRefreshingTrucks(true)
    setTrucksError(null)
    fetch('/api/trucks/refresh', { method: 'POST' })
      .then(r => {
        if (!r.ok) return r.json().then(e => Promise.reject(new Error(e.error || 'Refresh failed')))
        return r.json()
      })
      .then((data: Truck[]) => { setTrucks(data ?? []); setIsRefreshingTrucks(false) })
      .catch(err => { setTrucksError(err.message); setIsRefreshingTrucks(false) })
  }

  useEffect(() => {
    // Load cached data immediately, then auto-refresh to get fresh runs.
    fetchVehicles()
    fetchNewestRuns()
  }, [])

  const fetchCamTickets = (force = false) => {
    setIsRefreshingCamTickets(true)
    // force=true bypasses the 5-minute server-side cache via ?force=true query param.
    const url = force ? '/api/triage/camera-tickets?force=true' : '/api/triage/camera-tickets'
    fetch(url)
      .then(r => r.json())
      .then(d => setLatestCamTicketContent(d.content ?? ''))
      .catch(() => {})
      .finally(() => setIsRefreshingCamTickets(false))
  }

  useEffect(() => { fetchCamTickets() }, [])

  useEffect(() => {
    fetch('/api/me')
      .then(r => r.json())
      .then(d => setCurrentUserEmail(d.email ?? ''))
      .catch(() => {})
  }, [])

  // Keep selectedVehicle in sync after a refresh, but not for historical snapshot vehicles
  useEffect(() => {
    if (selectedVehicle && !isHistoricalVehicle) {
      const updated = vehicles.find(v => v.id === selectedVehicle.id)
      if (updated) setSelectedVehicle(updated)
    }
  }, [vehicles])

  const handleVehicleClick = (v: Vehicle, isHistorical?: boolean) => {
    setSelectedVehicle(v)
    setIsHistoricalVehicle(!!isHistorical)
  }

  const handleBack = () => {
    setSelectedVehicle(null)
    setIsHistoricalVehicle(false)
    setSelectedTruck(null)
  }

  const renderPage = () => {
    if (selectedTruck) {
      return (
        <TruckDetail
          truck={selectedTruck}
          onBack={() => setSelectedTruck(null)}
        />
      )
    }
    if (selectedVehicle) {
      return (
        <VehicleDetail
          vehicle={selectedVehicle}
          allVehicles={vehicles}
          backLabel={`Back to ${PAGE_LABELS[activePage] ?? 'triage'}`}
          onBack={handleBack}
          isHistorical={isHistoricalVehicle}
        />
      )
    }
    if (activePage === 'about') {
      return <AboutPage />
    }
    if (activePage === 'trucks') {
      return (
        <TrucksPage
          trucks={trucks}
          isLoading={isLoadingTrucks}
          isRefreshing={isRefreshingTrucks}
          error={trucksError}
          onRefresh={refreshTrucks}
          onTruckClick={t => setSelectedTruck(t)}
        />
      )
    }
    const projectFilter = activePage === 'oasis' ? 'Oasis' : activePage === 'neuron' ? 'Neuron' : undefined
    return (
      <MainContent
        vehicles={vehicles}
        isLoading={isLoading}
        isRefreshing={isRefreshing}
        error={error}
        onRefresh={fetchNewestRuns}
        onVehicleClick={(v, isHist) => handleVehicleClick(v, isHist)}
        projectFilter={projectFilter}
        latestCamTicketContent={latestCamTicketContent}
        onRefreshCamTickets={() => fetchCamTickets(true)}
        isRefreshingCamTickets={isRefreshingCamTickets}
        currentUserEmail={currentUserEmail}
      />
    )
  }

  return (
    <div className="flex h-screen bg-gray-100">
      <Sidebar activePage={activePage} onPageChange={(page) => {
        setSelectedVehicle(null)
        setSelectedTruck(null)
        setActivePage(page)
        if (page === 'trucks' && trucks.length === 0 && !isLoadingTrucks) {
          fetchTrucks()
        }
      }} />
      {renderPage()}
    </div>
  )
}

export default App
