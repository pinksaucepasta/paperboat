"use client"

import { PaletteIcon } from "@phosphor-icons/react"
import { terminalThemes, type TerminalTheme } from "@/lib/terminal/themes"
import { useTerminalTheme } from "@/lib/terminal/use-terminal-theme"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"

/** A theme's look in miniature: its background with red, green, yellow and blue on it. */
function Swatch({ theme }: { theme: TerminalTheme }) {
  return (
    <span
      className="flex h-4 w-7 shrink-0 items-center justify-center gap-px rounded-[3px] ring-1 ring-foreground/10"
      style={{ background: theme.background }}
      aria-hidden
    >
      {[1, 2, 3, 4].map((index) => (
        <span key={index} className="size-1 rounded-full" style={{ background: theme.ansi[index] }} />
      ))}
    </span>
  )
}

export function TerminalThemeMenu({ size = "icon-sm" }: { size?: "icon-sm" | "icon" }) {
  const { theme: current, choose } = useTerminalTheme()
  const dark = terminalThemes.filter((theme) => theme.appearance === "dark")
  const light = terminalThemes.filter((theme) => theme.appearance === "light")
  return (
    <DropdownMenu>
      <Tooltip>
        <TooltipTrigger
          render={
            <DropdownMenuTrigger
              render={
                <Button
                  size={size}
                  variant="ghost"
                  aria-label="Theme"
                  className="text-muted-foreground hover:text-foreground"
                />
              }
            />
          }
        >
          <PaletteIcon />
        </TooltipTrigger>
        <TooltipContent side="bottom">Theme</TooltipContent>
      </Tooltip>
      <DropdownMenuContent align="end" className="max-h-[min(32rem,var(--available-height))] w-60">
        <DropdownMenuRadioGroup value={current.id} onValueChange={choose}>
          {[
            { label: "Dark", themes: dark },
            { label: "Light", themes: light },
          ].map((group, index) => (
            <DropdownMenuGroup key={group.label}>
              {index > 0 && <DropdownMenuSeparator />}
              <DropdownMenuLabel>{group.label}</DropdownMenuLabel>
              {group.themes.map((theme) => (
                <DropdownMenuRadioItem key={theme.id} value={theme.id} className="gap-2.5">
                  <Swatch theme={theme} />
                  {theme.name}
                </DropdownMenuRadioItem>
              ))}
            </DropdownMenuGroup>
          ))}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
