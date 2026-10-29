import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { api } from "./lib/api";
import { computeHealth, worstLevel, type HealthItem, type HealthLevel } from "./lib/health";
import type { Bootstrap, DashboardData } from "./lib/types";
import { useLocale } from "./i18n/react";

interface AppState {
  boot: Bootstrap;
  reloadBoot: () => Promise<void>;
  dashboard: DashboardData | null;
  dashboardError: unknown;
  refreshDashboard: () => Promise<void>;
  /** Poll faster while a screen shows live data (now playing). */
  setLivePolling: (on: boolean) => void;
  health: HealthItem[];
  overall: HealthLevel;
}

const Ctx = createContext<AppState | null>(null);

const SLOW_MS = 30_000;
const FAST_MS = 5_000;

export function AppProvider({
  boot,
  reloadBoot,
  children,
}: {
  boot: Bootstrap;
  reloadBoot: () => Promise<void>;
  children: ReactNode;
}) {
  const [dashboard, setDashboard] = useState<DashboardData | null>(null);
  const [dashboardError, setDashboardError] = useState<unknown>(null);
  const [live, setLive] = useState(0);
  const inflight = useRef(false);

  const refreshDashboard = useCallback(async () => {
    if (inflight.current) return;
    inflight.current = true;
    try {
      setDashboard(await api.dashboard());
      setDashboardError(null);
    } catch (e) {
      setDashboardError(e);
    } finally {
      inflight.current = false;
    }
  }, []);

  useEffect(() => {
    void refreshDashboard();
  }, [refreshDashboard]);
  useEffect(() => {
    const ms = live > 0 ? FAST_MS : SLOW_MS;
    const t = window.setInterval(() => {
      if (document.visibilityState !== "hidden") void refreshDashboard();
    }, ms);
    return () => window.clearInterval(t);
  }, [live, refreshDashboard]);

  const setLivePolling = useCallback(
    (on: boolean) => setLive((n) => Math.max(0, n + (on ? 1 : -1))),
    [],
  );

  const { locale } = useLocale();
  const health = useMemo(() => computeHealth(dashboard?.sections), [dashboard, locale]);
  const overall = useMemo<HealthLevel>(() => {
    if (dashboardError) return "error";
    if (!dashboard) return "unknown";
    return worstLevel(health.map((h) => h.level));
  }, [health, dashboard, dashboardError]);

  const value = useMemo<AppState>(
    () => ({
      boot,
      reloadBoot,
      dashboard,
      dashboardError,
      refreshDashboard,
      setLivePolling,
      health,
      overall,
    }),
    [
      boot,
      reloadBoot,
      dashboard,
      dashboardError,
      refreshDashboard,
      setLivePolling,
      health,
      overall,
    ],
  );

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useApp(): AppState {
  const v = useContext(Ctx);
  if (!v) throw new Error("useApp must be used inside AppProvider");
  return v;
}

/** The app state when rendered inside the shell, null otherwise (standalone screens, tests). */
export function useAppOptional(): AppState | null {
  return useContext(Ctx);
}

/** Keep fast polling on while the calling screen is mounted. */
export function useLiveDashboard(): void {
  const { setLivePolling } = useApp();
  useEffect(() => {
    setLivePolling(true);
    return () => setLivePolling(false);
  }, [setLivePolling]);
}
