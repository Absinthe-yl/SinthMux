import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createContext, useContext, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { APIClient } from "./api";

const APIContext = createContext<APIClient | null>(null);

export function useAPI() {
  const api = useContext(APIContext);
  if (!api) throw new Error("缺少 API 作用域");
  return api;
}

// 由调用方按身份和登录生命周期加 key，卸载时取消请求并销毁整个账号缓存。
export function QueryScope({ children, csrf = "", onUnauthorized }: { children: ReactNode; csrf?: string; onUnauthorized?: () => void }) {
  const [client] = useState(() => new QueryClient());
  const [api] = useState(() => new APIClient());
  const unauthorized = useRef(onUnauthorized);
  useLayoutEffect(() => { unauthorized.current = onUnauthorized; });
  useLayoutEffect(() => {
    api.activate(csrf);
    api.onUnauthorized = () => {
      if (!unauthorized.current) return;
      api.dispose();
      client.clear();
      unauthorized.current();
    };
    return () => { api.dispose(); client.clear(); };
  }, [api, client, csrf]);
  return <APIContext.Provider value={api}><QueryClientProvider client={client}>{children}</QueryClientProvider></APIContext.Provider>;
}
