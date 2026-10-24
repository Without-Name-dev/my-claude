import type { Metadata } from "next"

import { PrivacyPolicyScreen } from "@/components/pages/privacy-policy-screen"

export const metadata: Metadata = {
  title: "Privacy policy · My Claude",
  description:
    "How the My Claude web and desktop clients handle information.",
}

export default function PrivacyPage() {
  return <PrivacyPolicyScreen />
}
