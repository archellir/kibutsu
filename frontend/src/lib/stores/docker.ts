import { writable, derived } from 'svelte/store';
import type { Container, Image, ComposeProject, SystemInfo, DockerError, DiskUsage } from '../types/docker';
import { DockerClient } from '../api/client';

const client = new DockerClient();

// Create base store type
type StoreState<T> = {
  data: T;
  loading: boolean;
  lastUpdated: Date | null;
};

// Create base stores with loading states
const createLoadingStore = <T>() => {
  const { subscribe, set: baseSet, update } = writable<StoreState<T>>({
    data: [] as unknown as T,
    loading: false,
    lastUpdated: null
  });

  return {
    subscribe,
    set: (data: T) => baseSet({
      data,
      loading: false,
      lastUpdated: new Date()
    }),
    setLoading: (loading: boolean) => update(store => ({ ...store, loading })),
    refresh: async (fetchFn: () => Promise<T>) => {
      update(store => ({ ...store, loading: true }));
      try {
        const data = await fetchFn();
        baseSet({
          data,
          loading: false,
          lastUpdated: new Date()
        });
      } catch (error: unknown) {
        errorStore.add({
          message: error instanceof Error ? error.message : 'Unknown error',
          code: 'FETCH_ERROR',
          timestamp: new Date()
        });
        update(store => ({ ...store, loading: false }));
      }
    }
  };
};

// Create stores
export const containersStore = createLoadingStore<Container[]>();
export const imagesStore = createLoadingStore<Image[]>();
export const composeStore = createLoadingStore<ComposeProject[]>();
export const systemStore = createLoadingStore<SystemInfo>();
export const diskUsageStore = createLoadingStore<DiskUsage>();

// Error store with history
export const errorStore = (() => {
  const { subscribe, update } = writable<DockerError[]>([]);

  return {
    subscribe,
    add: (error: DockerError) => update(errors => [error, ...errors].slice(0, 10)),
    clear: () => update(() => [])
  };
})();

// WebSocket connection management
function createWebSocketConnection() {
  if (typeof window === 'undefined') {
    return null;
  }

  const wsUrl = client.getWebSocketUrl();
  if (!wsUrl) {
    console.error('WebSocket URL is not available');
    return null;
  }

  try {
    const ws = new WebSocket(wsUrl);
    
    ws.onmessage = (event) => {
      try {
        const data = JSON.parse(event.data);
        handleWebSocketMessage(data);
      } catch (error) {
        console.error('Failed to parse WebSocket message:', error);
      }
    };
    
    ws.onclose = () => {
      setTimeout(createWebSocketConnection, 5000);
    };

    ws.onerror = (error) => {
      console.error('WebSocket error:', error);
    };
    
    return ws;
  } catch (error) {
    console.error('Failed to create WebSocket connection:', error);
    return null;
  }
}

// WebSocket message handler
function handleWebSocketMessage(data: any) {
  switch (data.type) {
    case 'container':
      containersStore.refresh(() => client.getContainers());
      break;
    case 'image':
      imagesStore.refresh(() => client.getImages());
      break;
    case 'compose':
      composeStore.refresh(() => client.getComposeProjects());
      break;
    case 'system':
      systemStore.refresh(() => client.getSystemInfo());
      break;
  }
}

// Auto-refresh functionality
const setupAutoRefresh = () => {
  const refreshInterval = 30000; // 30 seconds

  const refresh = async () => {
    await Promise.all([
      containersStore.refresh(() => client.getContainers()),
      imagesStore.refresh(() => client.getImages()),
      composeStore.refresh(() => client.getComposeProjects()),
      systemStore.refresh(() => client.getSystemInfo()),
      diskUsageStore.refresh(() => client.getDiskUsage())
    ]);
  };

  // Initial load
  refresh();

  // Set up interval for system metrics
  const interval = setInterval(() => {
    systemStore.refresh(() => client.getSystemInfo());
  }, refreshInterval);

  // Set up WebSocket
  const ws = createWebSocketConnection();

  // Cleanup function
  return () => {
    clearInterval(interval);
    ws?.close();
  };
};

// Initialize auto-refresh if in browser
if (typeof window !== 'undefined') {
  setupAutoRefresh();
} 