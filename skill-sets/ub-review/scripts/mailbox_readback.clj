(require '[clojure.edn :as edn]
         '[clojure.string :as string]
         '[uml-viewer.domain.mailbox :as mailbox])
(let [root (first *command-line-args*)
      file (mailbox/to-agent root)]
  (when (.isFile file)
    ;; Validate original bytes before the upstream convenience reader can hide a parse error.
    (edn/read-string (slurp file)))
  (println (string/join "," (map :id (:queue (mailbox/read-mailbox file))))))
