import { Shell } from "@/components/shell"

export default function PanelLayout({ children }: LayoutProps<"/">) {
  return <Shell>{children}</Shell>
}
