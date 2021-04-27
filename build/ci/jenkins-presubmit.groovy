// This is a 'Jenkinsfile'-style declarative 'Pipeline' definition. It is
// executed by Jenkins for presubmit checks, ie. checks that run against an
// open Gerrit change request.

pipeline {
    agent none
    stages {
        stage('Parallel') {
            parallel {
                stage('Test') {
                    agent {
                        node {
                            label ""
                            customWorkspace '/home/ci/monogon'
                        }
                    }
                    steps {
                        gerritCheck checks: ['jenkins:test': 'RUNNING'], message: "Running on ${env.NODE_NAME}"
                        sh "git clean -fdx -e '/bazel-*'"
                        sh "bazel test //..."
                        sh "bazel test -c dbg //..."
                    }
                    post {
                        success {
                            gerritCheck checks: ['jenkins:test': 'SUCCESSFUL']
                        }
                        unsuccessful {
                            gerritCheck checks: ['jenkins:test': 'FAILED']
                        }
                    }
                }

                stage('Gazelle') {
                    agent {
                        node {
                            label ""
                            customWorkspace '/home/ci/monogon'
                        }
                    }
                    steps {
                        gerritCheck checks: ['jenkins:gazelle': 'RUNNING'], message: "Running on ${env.NODE_NAME}"
                        sh "git clean -fdx -e '/bazel-*'"
                        sh "bazel run //:fietsje"
                        sh "bazel run //:gazelle -- update"

                        script {
                            def diff = sh script: "git status --porcelain", returnStdout: true
                            if (diff.trim() != "") {
                                sh "git diff HEAD"
                                error """
                                    Unclean working directory after running gazelle and Fietsje.
                                    Please run:

                                       \$ bazel run //:fietsje
                                       \$ bazel run //:gazelle -- update

                                    In your git checkout and amend the resulting diff to this changelist.
                                """
                            }
                        }
                    }
                    post {
                        success {
                            gerritCheck checks: ['jenkins:gazelle': 'SUCCESSFUL']
                        }
                        unsuccessful {
                            gerritCheck checks: ['jenkins:gazelle': 'FAILED']
                        }
                    }
                }
            }
        }
    }
}