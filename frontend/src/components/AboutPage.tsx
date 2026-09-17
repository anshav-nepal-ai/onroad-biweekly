const AboutPage = () => (
  <div className="flex-1 overflow-auto">
    <div className="p-8 max-w-3xl">
      <h1 className="text-3xl font-bold text-gray-900 mb-2">About</h1>
      <p className="text-gray-500 text-sm mb-8">Onroad QC — bi-weekly data quality triage report</p>

      <div className="space-y-6">
        <div className="bg-white rounded-lg border border-gray-200 p-6">
          <h2 className="text-base font-semibold text-gray-900 mb-3">What this app does</h2>
          <p className="text-sm text-gray-600 leading-relaxed">
            Onroad QC is a collaborative bi-weekly triage report for assessing camera image quality and
            drive quality across the onroad vehicle fleet. It surfaces the latest run snippets per vehicle,
            tracks per-camera calibration staleness, and lets the team annotate each vehicle with quality
            ratings and triage comments. Annotations, action items, and triage history are persisted in a
            shared database — anyone can view, and one editor at a time can update. When triage is ready,
            the formatted summary can be posted to #eng-sds-data-qa via the <code className="bg-gray-100 px-1 rounded text-xs">/post-triage-slack</code> skill in Claude.
          </p>
        </div>

        <div className="bg-white rounded-lg border border-gray-200 p-6">
          <h2 className="text-base font-semibold text-gray-900 mb-3">Data sources</h2>
          <ul className="space-y-2 text-sm text-gray-600">
            <li><span className="font-medium text-gray-800">Vehicles</span> — Fleetio fleet roster via the backend refresh</li>
            <li><span className="font-medium text-gray-800">Runs &amp; snippets</span> — fetched from ADP SQL + S3</li>
            <li><span className="font-medium text-gray-800">Calibration staleness</span> — ADP data; per-camera last-calibrated dates with overdue flagging</li>
            <li><span className="font-medium text-gray-800">Triage annotations</span> — stored in Cloud SQL, shared across all users, persisted across triage cycles</li>
            <li><span className="font-medium text-gray-800">Active VSTAB tickets</span> — fetched from Jira when a new triage cycle is started and seeded into the Action Items section</li>
          </ul>
        </div>

        <div className="bg-white rounded-lg border border-gray-200 p-6">
          <h2 className="text-base font-semibold text-gray-900 mb-3">Triage workflow</h2>
          <ol className="space-y-2 text-sm text-gray-600 list-decimal list-inside">
            <li>Click <span className="font-medium text-gray-800">New Triage</span> to start a fresh cycle — enter your name and email; active VSTAB tickets are auto-seeded into Action Items</li>
            <li>Click <span className="font-medium text-gray-800">Edit</span> and enter your name and email to begin annotating (one editor at a time; 30-minute auto-timeout)</li>
            <li>For each vehicle, click the <span className="font-medium text-gray-800">Cam Quality</span> cell and select good / degraded / bad, with an optional note</li>
            <li>Click the <span className="font-medium text-gray-800">Drive Quality</span> cell and select good / bad</li>
            <li>Add free-text notes in the <span className="font-medium text-gray-800">Triage Comments</span> column</li>
            <li>Fill in the <span className="font-medium text-gray-800">Action Items</span> section above the table (New, Ongoing, Camera Quality, Active Tickets)</li>
            <li>Click <span className="font-medium text-gray-800">Done Editing</span> when finished — other team members can then take over</li>
            <li>Use <span className="font-medium text-gray-800">Preview Slack</span> to review the message, then run <code className="bg-gray-100 px-1 rounded text-xs">/post-triage-slack</code> in Claude to post to #eng-sds-data-qa</li>
          </ol>
        </div>

        <div className="bg-white rounded-lg border border-gray-200 p-6">
          <h2 className="text-base font-semibold text-gray-900 mb-3">Tips</h2>
          <ul className="space-y-2 text-sm text-gray-600">
            <li>All annotations auto-save immediately on each change — no submit button needed</li>
            <li>The cycle history dropdown lets anyone browse past triage cycles without affecting the current one</li>
            <li>Click <span className="font-medium text-gray-800">Fetch Newest Runs</span> to pull the latest run data — this only updates vehicle run info, never touches annotations</li>
            <li>Click any vehicle label to open the detail view with recent run snippets and the full per-camera calibration table</li>
            <li>Overdue calibrations are highlighted in red in both the triage table and the vehicle detail view</li>
            <li>Action items use the format <code className="bg-gray-100 px-1 rounded text-xs">text | https://url</code> to include a Jira link — it renders as a link in the Slack message</li>
          </ul>
        </div>
      </div>
    </div>
  </div>
)

export default AboutPage
